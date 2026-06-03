import express from 'express';
import { v4 as uuidv4 } from 'uuid';
import {
  DefaultRequestHandler,
  InMemoryTaskStore,
  type AgentExecutor,
  type ExecutionEventBus,
  RequestContext,
} from '@a2a-js/sdk/server';
import {
  jsonRpcHandler,
  agentCardHandler,
  UserBuilder,
} from '@a2a-js/sdk/server/express';
import type { AgentCard, Message } from '@a2a-js/sdk';
import { configureProvider } from '@flue/runtime/app';
import {
  createFlueContext,
  InMemorySessionStore,
  resolveModel,
} from '@flue/runtime/internal';
import { local } from '@flue/runtime/node';
import { postEvent, instrumentFlue, promptTimeoutSignal, TokenClient, tenantHeader } from '@agent-platform/sdk';

const agentId     = process.env.AGENT_ID      ?? 'unknown';
const registryUrl  = process.env.REGISTRY_URL  ?? 'http://registry:8080';
const aiProvider   = process.env.AI_PROVIDER   ?? 'anthropic';
const aiApiKey     = process.env.AI_API_KEY    ?? '';
const aiModel      = process.env.AI_MODEL      ?? 'anthropic/claude-sonnet-4-6';
const promptTimeoutMs = Number(process.env.PROMPT_TIMEOUT_MS ?? 120000);

const sidecarUrl = process.env.SIDECAR_URL ?? 'http://localhost:8089';
const apiBUrl    = process.env.API_B_URL   ?? 'http://sample-api-b:8080';
const tenantId   = process.env.TENANT_ID   || undefined;

// Metadata key the parent uses to carry the delegation token over A2A.
const DELEGATION_METADATA_KEY = 'delegationToken';

configureProvider(aiProvider, { apiKey: aiApiKey });

// Sidecar token client (handles the SVID-not-ready retry and RFC 8693 exchange).
// See @agent-platform/sdk.
const tokens = new TokenClient(sidecarUrl);

// Extract the delegation token from an incoming A2A message: prefer message
// metadata, fall back to any text part shaped as "delegationToken=<...>".
function extractDelegationToken(message: Message | undefined): string | undefined {
  if (!message) return undefined;
  const metaVal = message.metadata?.[DELEGATION_METADATA_KEY];
  if (typeof metaVal === 'string' && metaVal.length > 0) {
    return metaVal;
  }
  for (const part of message.parts ?? []) {
    if (part.kind === 'text' && typeof part.text === 'string') {
      const prefix = `${DELEGATION_METADATA_KEY}=`;
      if (part.text.startsWith(prefix)) {
        return part.text.slice(prefix.length).trim();
      }
    }
  }
  return undefined;
}

// Run the full delegation flow: exchange the delegation token, GET API-B
// (expected 200), then POST API-B with the same read-scoped token (expected
// 403, demonstrating attenuation). Never throws; returns a summary string.
async function runDelegationFlow(delegationToken: string): Promise<string> {
  let exchanged: string;
  try {
    exchanged = await tokens.exchangeToken({
      subjectToken: delegationToken,
      audience: 'sample-api-b',
      scope: 'sample-api-b:read',
    });
  } catch (err) {
    console.error('[child-agent] token exchange failed:', err);
    await postEvent(registryUrl, agentId, 'delegation_exchange', {
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    });
    return `delegation exchange failed: ${err instanceof Error ? err.message : String(err)}`;
  }
  await postEvent(registryUrl, agentId, 'delegation_exchange', {
    ok: true,
    audience: 'sample-api-b',
    scope: 'sample-api-b:read',
  });

  // (b) GET API-B /work — expected 200 with the exchanged token.
  let readActingAgent: unknown;
  let readStatus = 0;
  try {
    const res = await fetch(`${apiBUrl}/work`, {
      method: 'GET',
      headers: { Authorization: `Bearer ${exchanged}`, ...tenantHeader(tenantId) },
    });
    readStatus = res.status;
    let body: any;
    try {
      body = await res.json();
    } catch {
      body = await res.text().catch(() => '');
    }
    readActingAgent = body?.agentName;
    console.log(`[child-agent] API-B GET /work -> ${res.status}`);
    await postEvent(registryUrl, agentId, 'api_b_call', {
      method: 'GET',
      status: res.status,
      ok: res.ok,
      actingAgent: body?.agentName,
      response: body,
    });
  } catch (err) {
    console.error('[child-agent] API-B GET failed:', err);
    await postEvent(registryUrl, agentId, 'api_b_call', {
      method: 'GET',
      status: 0,
      ok: false,
      error: err instanceof Error ? err.message : String(err),
    });
  }

  // (c) POST API-B /work with the SAME read-scoped token — expected 403,
  // demonstrating scope attenuation (write denied).
  let writeStatus = 0;
  try {
    const res = await fetch(`${apiBUrl}/work`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${exchanged}`,
        ...tenantHeader(tenantId),
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({}),
    });
    writeStatus = res.status;
    let body: unknown;
    try {
      body = await res.json();
    } catch {
      body = await res.text().catch(() => '');
    }
    console.log(`[child-agent] API-B POST /work -> ${res.status} (expected 403)`);
    await postEvent(registryUrl, agentId, 'api_b_write_denied', {
      method: 'POST',
      status: res.status,
      expected: 403,
      denied: res.status === 403,
      response: body,
    });
  } catch (err) {
    console.error('[child-agent] API-B POST failed:', err);
    await postEvent(registryUrl, agentId, 'api_b_write_denied', {
      method: 'POST',
      status: 0,
      error: err instanceof Error ? err.message : String(err),
    });
  }

  return (
    `API-B read status=${readStatus} actingAgent=${String(readActingAgent)}; ` +
    `API-B write status=${writeStatus} (expected 403, attenuated read-only token)`
  );
}

const agentCard: AgentCard = {
  name: 'child-agent',
  description: 'A child agent that generates a random string using Claude',
  url: `http://${agentId}-svc:8080`,
  version: '1.0.0',
  protocolVersion: '0.2.6',
  capabilities: {
    streaming: false,
    pushNotifications: false,
    stateTransitionHistory: false,
  },
  defaultInputModes: ['text/plain'],
  defaultOutputModes: ['text/plain'],
  skills: [
    {
      id: 'generate-random-string',
      name: 'Generate Random String',
      description: 'Generates a random 8-character alphanumeric string',
      tags: ['random', 'string'],
    },
  ],
};

class ChildAgentExecutor implements AgentExecutor {
  async execute(requestContext: RequestContext, eventBus: ExecutionEventBus): Promise<void> {
    try {
      // Deterministic delegation flow: if the parent passed a delegation token,
      // exchange it and call API-B (read ok, write denied). Done before the LLM
      // step so the acceptance test does not depend on model behavior.
      const delegationToken = extractDelegationToken(requestContext.userMessage);
      let delegationSummary = '';
      if (delegationToken) {
        console.log('[child-agent] received delegation token; running delegation flow');
        delegationSummary = await runDelegationFlow(delegationToken);
      } else {
        console.log('[child-agent] no delegation token in incoming message');
      }

      // Build a minimal FlueContext to use Flue's session.prompt()
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

      const harness = await ctx.init({ model: aiModel, sandbox: local() });
      const session = await harness.session();
      const response = await session.prompt(
        'Generate a random 8-character alphanumeric string. Reply with ONLY the string itself, no explanation.',
        { signal: promptTimeoutSignal(promptTimeoutMs) }
      );
      const randomString = response.text.trim();

      // Post event to registry
      await postEvent(registryUrl, agentId, 'child_result', { result: randomString });

      // Reply text: include the delegation summary when delegation ran, while
      // keeping the random string so existing behavior still works.
      const text = delegationSummary
        ? `${delegationSummary} | randomString=${randomString}`
        : randomString;

      // Publish A2A reply
      const replyMessage: Message = {
        kind: 'message',
        messageId: uuidv4(),
        role: 'agent',
        parts: [{ kind: 'text', text }],
        contextId: requestContext.contextId,
        taskId: requestContext.taskId,
      };
      eventBus.publish(replyMessage);
      eventBus.finished();

      // Exit after response is sent
      setTimeout(() => process.exit(0), 500);
    } catch (err) {
      console.error('[child-agent] executor error:', err);
      await postEvent(registryUrl, agentId, 'agent_error', {
        error: err instanceof Error ? err.message : String(err),
        stack: err instanceof Error ? err.stack : undefined,
      });
      const errorMessage: Message = {
        kind: 'message',
        messageId: uuidv4(),
        role: 'agent',
        parts: [{ kind: 'text', text: `Error: ${String(err)}` }],
        contextId: requestContext.contextId,
        taskId: requestContext.taskId,
      };
      eventBus.publish(errorMessage);
      eventBus.finished();
      setTimeout(() => process.exit(1), 500);
    }
  }

  async cancelTask(_taskId: string, eventBus: ExecutionEventBus): Promise<void> {
    eventBus.finished();
  }
}

const taskStore = new InMemoryTaskStore();
const executor = new ChildAgentExecutor();
const requestHandler = new DefaultRequestHandler(agentCard, taskStore, executor);

const app = express();
app.use(express.json());

// Serve agent card at /.well-known/agent.json (also serve at standard path).
// agentCardHandler() returns a Router whose route is `GET /`, so it must be
// MOUNTED with app.use — app.get(exactPath, router) never matches the router's
// internal `/` and 404s, which silently breaks agent-card discovery.
app.use('/.well-known/agent.json', agentCardHandler({ agentCardProvider: requestHandler }));
app.use('/.well-known/agent-card.json', agentCardHandler({ agentCardProvider: requestHandler }));

// JSON-RPC handler at root
app.post('/', jsonRpcHandler({ requestHandler, userBuilder: UserBuilder.noAuthentication }));

const PORT = 8080;
app.listen(PORT, () => {
  console.log(`[child-agent] ${agentId} listening on :${PORT}`);
});
