import { v4 as uuidv4 } from 'uuid';
import { ClientFactory } from '@a2a-js/sdk/client';
import type { ToolDef } from '@flue/runtime';
import { configureProvider } from '@flue/runtime/app';
import {
  createFlueContext,
  InMemorySessionStore,
  resolveModel,
} from '@flue/runtime/internal';
import { local } from '@flue/runtime/node';
import { postEvent, instrumentFlue, promptTimeoutSignal, TokenClient, tenantHeader } from '@spawnly/sdk';

const agentId       = process.env.AGENT_ID        ?? 'unknown';
const registryUrl    = process.env.REGISTRY_URL    ?? 'http://registry:8080';
const orchestratorUrl = process.env.ORCHESTRATOR_URL ?? 'http://orchestrator:8080';
const tenantId       = process.env.TENANT_ID       || undefined;
const userId         = process.env.USER_ID         ?? 'unknown';
const aiProvider     = process.env.AI_PROVIDER     ?? 'anthropic';
const aiApiKey       = process.env.AI_API_KEY      ?? '';
const aiModel        = process.env.AI_MODEL        ?? 'anthropic/claude-sonnet-4-6';
const promptTimeoutMs = Number(process.env.PROMPT_TIMEOUT_MS ?? 120000);

const sidecarUrl = process.env.SIDECAR_URL ?? 'http://localhost:8089';
const apiAUrl    = process.env.API_A_URL   ?? 'http://sample-api-a:8080';

// Metadata key used to carry the delegation token to the child over A2A.
const DELEGATION_METADATA_KEY = 'delegationToken';

configureProvider(aiProvider, { apiKey: aiApiKey });

// Sidecar token client (handles the SVID-not-ready retry and the three /token
// modes). See @spawnly/sdk.
const tokens = new TokenClient(sidecarUrl);

// The delegation token minted in main() and passed to the child via A2A.
// Module-level so the call_child_agent tool can read it deterministically
// without depending on the LLM to thread it through.
let delegationToken: string | undefined;
// The child's API-B result text, captured so main() can summarize it.
let childResultText = '';

// Tool: spawn a child agent
const spawnChildAgent: ToolDef = {
  name: 'spawn_child_agent',
  description: 'Spawn a new currency-converter child agent instance. Returns the child agent ID.',
  parameters: {
    type: 'object',
    properties: {},
    required: [],
  },
  execute: async (_args: Record<string, unknown>) => {
    const res = await fetch(`${orchestratorUrl}/spawn`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        agentType: 'currency-converter',
        tenantId,
        userId,
        parentId: agentId,
      }),
    });
    if (!res.ok) {
      throw new Error(`spawn failed: ${res.status} ${await res.text()}`);
    }
    const data = (await res.json()) as { workloadName?: string; id?: string; [k: string]: unknown };
    const childId = data.workloadName ?? data.id ?? String(data);
    return JSON.stringify({ childId });
  },
};

// Tool: wait for child agent to be ready (polls /.well-known/agent.json)
const waitForChildReady: ToolDef = {
  name: 'wait_for_child_ready',
  description: 'Poll until the child agent service is reachable. Pass childId from spawn_child_agent.',
  parameters: {
    type: 'object',
    properties: {
      childId: { type: 'string', description: 'The child agent ID returned by spawn_child_agent' },
    },
    required: ['childId'],
  },
  execute: async (args: Record<string, unknown>) => {
    const childId = String(args.childId);
    const url = `http://${childId}-svc:8080/.well-known/agent.json`;
    const deadline = Date.now() + 60_000;
    while (Date.now() < deadline) {
      try {
        const res = await fetch(url);
        if (res.ok) {
          return JSON.stringify({ ready: true });
        }
      } catch {
        // not ready yet
      }
      await new Promise((r) => setTimeout(r, 2_000));
    }
    throw new Error(`Timed out waiting for child agent ${childId} to be ready`);
  },
};

// Tool: call the child agent via A2A
const callChildAgent: ToolDef = {
  name: 'call_child_agent',
  description: 'Send a message to the child agent via A2A and return its text response.',
  parameters: {
    type: 'object',
    properties: {
      childId: { type: 'string', description: 'The child agent ID' },
    },
    required: ['childId'],
  },
  execute: async (args: Record<string, unknown>) => {
    const childId = String(args.childId);
    const factory = new ClientFactory();
    const client = await factory.createFromUrl(`http://${childId}-svc:8080`);
    const result = await client.sendMessage({
      message: {
        kind: 'message',
        messageId: uuidv4(),
        role: 'user',
        parts: [{ kind: 'text', text: 'Convert 100 USD to EUR.' }],
        // Pass the delegation token to the child via message metadata so it can
        // exchange it for a sample-api-b token (RFC 8693 act-chain extension).
        metadata: delegationToken
          ? { [DELEGATION_METADATA_KEY]: delegationToken }
          : undefined,
      },
    });
    // Extract text from response (Message or Task)
    let text = '';
    if ('kind' in result && result.kind === 'message') {
      for (const part of result.parts) {
        if (part.kind === 'text') {
          text += part.text;
        }
      }
    } else if ('kind' in result && result.kind === 'task') {
      const task = result as { kind: 'task'; history?: Array<{ role: string; parts: Array<{ kind: string; text?: string }> }> };
      const history = task.history ?? [];
      for (const msg of history) {
        if (msg.role === 'agent') {
          for (const part of msg.parts) {
            if (part.kind === 'text' && part.text) {
              text += part.text;
            }
          }
        }
      }
    }
    childResultText = text;
    return JSON.stringify({ result: text });
  },
};

// Tool: kill the child agent
const killChildAgent: ToolDef = {
  name: 'kill_child_agent',
  description: 'Terminate the child agent by calling the orchestrator DELETE endpoint.',
  parameters: {
    type: 'object',
    properties: {
      childId: { type: 'string', description: 'The child agent ID to terminate' },
    },
    required: ['childId'],
  },
  execute: async (args: Record<string, unknown>) => {
    const childId = String(args.childId);
    const res = await fetch(`${orchestratorUrl}/v1/agents/${childId}`, {
      method: 'DELETE',
    });
    if (!res.ok && res.status !== 404) {
      throw new Error(`delete failed: ${res.status} ${await res.text()}`);
    }
    return JSON.stringify({ done: true });
  },
};

// Deterministic step (a): get a working token for API-A and call it directly.
async function callApiADirect(): Promise<void> {
  try {
    const token = await tokens.getToken('sample-api-a:read sample-api-a:write');
    const res = await fetch(`${apiAUrl}/work`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        ...tenantHeader(tenantId),
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({}),
    });
    let body: unknown;
    try {
      body = await res.json();
    } catch {
      body = await res.text().catch(() => '');
    }
    console.log(`[trip-planner] API-A POST /work -> ${res.status}`);
    await postEvent(registryUrl, agentId, 'api_a_call', {
      method: 'POST',
      status: res.status,
      ok: res.ok,
      response: body,
    });
  } catch (err) {
    console.error('[trip-planner] API-A call failed:', err);
    await postEvent(registryUrl, agentId, 'api_a_call', {
      method: 'POST',
      status: 0,
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    });
  }
}

// Deterministic step (b): mint a delegation token scoped to sample-api-b:read.
async function mintDelegationToken(): Promise<void> {
  try {
    delegationToken = await tokens.getToken('sample-api-b:read', { audience: 'delegation' });
    console.log('[trip-planner] minted delegation token for sample-api-b:read');
    await postEvent(registryUrl, agentId, 'delegation_token_minted', {
      audience: 'delegation',
      scope: 'sample-api-b:read',
      ok: true,
    });
  } catch (err) {
    console.error('[trip-planner] delegation token mint failed:', err);
    await postEvent(registryUrl, agentId, 'delegation_token_minted', {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    });
  }
}

async function main() {
  await postEvent(registryUrl, agentId, 'parent_started', { agentId });

  // Deterministic delegation setup, performed BEFORE the LLM orchestration so
  // the delegation token is available when call_child_agent runs.
  await callApiADirect();
  await mintDelegationToken();

  const sessionStore = new InMemorySessionStore();
  const ctx = createFlueContext({
    id: agentId,
    runId: uuidv4(),
    payload: {},
    env: process.env as Record<string, string>,
    agentConfig: {
      systemPrompt: '',
      skills: {},
      roles: {},
      model: undefined,
      resolveModel,
    },
    createDefaultEnv: async () => local().createSessionEnv({ id: agentId, cwd: process.cwd() }),
    defaultStore: sessionStore,
  });
  instrumentFlue(ctx, registryUrl, agentId);

  const harness = await ctx.init({
    model: aiModel,
    tools: [spawnChildAgent, waitForChildReady, callChildAgent, killChildAgent],
    sandbox: local(),
  });

  const session = await harness.session();
  const response = await session.prompt(
    'You are a trip planner preparing a traveler\'s budget. ' +
    'Use spawn_child_agent to start the currency converter, ' +
    'then use wait_for_child_ready to wait until it is reachable, ' +
    'then use call_child_agent to ask it to convert the trip budget and receive the conversion result, ' +
    'then use kill_child_agent to terminate it. ' +
    'Finally, report back the converted amount that the currency converter produced.',
    { signal: promptTimeoutSignal(promptTimeoutMs) }
  );

  // Deterministic step (d): summarize the child's returned API-B result.
  await postEvent(registryUrl, agentId, 'child_delegation_summary', {
    childResult: childResultText,
    delegationDelivered: Boolean(delegationToken),
  });

  await postEvent(registryUrl, agentId, 'parent_completed', { result: response.text });
  console.log('[trip-planner] completed. result:', response.text);
  console.log('[trip-planner] child delegation result:', childResultText);
}

main().catch(async (err) => {
  console.error('[trip-planner] fatal error:', err);
  await postEvent(registryUrl, agentId, 'agent_error', {
    error: err instanceof Error ? err.message : String(err),
    stack: err instanceof Error ? err.stack : undefined,
  });
  process.exit(1);
});
