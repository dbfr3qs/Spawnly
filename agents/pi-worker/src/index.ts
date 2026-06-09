// pi-worker — a long-lived, chat-enabled Spawnly agent built on the Pi coding
// harness (@earendil-works/pi-coding-agent), proving the platform contract is
// agent-framework-agnostic: this agent talks to the platform using ONLY the
// existing @spawnly/sdk surface (postEvent / TokenClient / createAuthenticatedFetch)
// and the existing chat HTTP contract. Nothing under cmd/, internal/, or sdks/
// is modified for Pi.
//
// Chat contract (unchanged, identical to the Flue agents): the orchestrator
// forwards dashboard messages to POST /agents/chat/:sessionId with a JSON body
// { message, sessionId } and relays back whatever JSON we return ({ response }).

import express from 'express';
import {
  AuthStorage,
  ModelRegistry,
  SessionManager,
  createAgentSession,
  type AgentSession,
} from '@earendil-works/pi-coding-agent';
import { getModel } from '@earendil-works/pi-ai';
import { postEvent } from '@spawnly/sdk';
import { instrumentPiSession } from './instrument.js';
import { makeProtectedApiTool } from './tools.js';

// --- Platform-injected env contract (set by the operator from the template) ---
const agentId = process.env.AGENT_ID ?? 'unknown';
const registryUrl = process.env.REGISTRY_URL ?? 'http://registry:8080';
const aiApiKey = process.env.AI_API_KEY ?? '';
const aiModel = process.env.AI_MODEL ?? 'openai/gpt-4o';
const promptTimeoutMs = Number(process.env.PROMPT_TIMEOUT_MS ?? 180_000);

const sidecarUrl = process.env.SIDECAR_URL ?? 'http://localhost:8089';
const sampleApiUrl = process.env.SAMPLE_API_URL ?? 'http://sample-api-global';
const scope = process.env.SCOPE ?? 'sample-api-a:read';
const tenantId = process.env.TENANT_ID || undefined;
const workDir = process.env.PI_WORKDIR ?? '/work';

// AI_MODEL is "provider/id" (e.g. "openai/gpt-4o"); split it for Pi's getModel.
const slash = aiModel.indexOf('/');
const provider = slash > 0 ? aiModel.slice(0, slash) : 'openai';
const modelId = slash > 0 ? aiModel.slice(slash + 1) : aiModel;

console.log(`[pi-worker] ${agentId} starting — provider=${provider} model=${modelId} cwd=${workDir}`);

// Pi reads the API key from AuthStorage. Inject the platform-provided key as a
// runtime override (never persisted to disk) so there is no interactive login.
const authStorage = AuthStorage.create();
if (aiApiKey) authStorage.setRuntimeApiKey(provider, aiApiKey);
const modelRegistry = ModelRegistry.create(authStorage);
// provider/modelId come from the AI_MODEL env string; getModel is typed for
// known-provider/model literals, so call it untyped — an unknown pair simply
// yields undefined and we fall back to Pi's default model below.
const model = (getModel as (p: string, id: string) => ReturnType<typeof getModel>)(provider, modelId);
if (!model) {
  console.warn(`[pi-worker] getModel("${provider}","${modelId}") returned nothing; Pi will pick a default available model`);
}

// The one Spawnly-identity-backed tool we hand Pi alongside its coding tools.
const protectedTool = makeProtectedApiTool({ sampleApiUrl, scope, sidecarUrl, tenantId });

// --- Heartbeat: long-lived liveness signal for the dashboard ----------------
async function beat(): Promise<void> {
  await postEvent(registryUrl, agentId, 'heartbeat', {
    status: 'running',
    timestamp: new Date().toISOString(),
  });
}
void beat();
setInterval(() => void beat(), 30_000);

// --- One Pi session per chat sessionId --------------------------------------
interface PiHandle {
  session: AgentSession;
  // Mutable buffer the response-capture listener accumulates the final
  // assistant text into; reset at the start of every turn.
  capture: { text: string };
}

const sessions = new Map<string, Promise<PiHandle>>();

function getSession(sessionId: string): Promise<PiHandle> {
  const existing = sessions.get(sessionId);
  if (existing) return existing;

  const created = (async (): Promise<PiHandle> => {
    const { session } = await createAgentSession({
      ...(model ? { model } : {}),
      authStorage,
      modelRegistry,
      cwd: workDir,
      sessionManager: SessionManager.inMemory(workDir),
      // The `tools` allowlist is matched against ALL available tools (built-in
      // AND custom), so the custom tool's name must be included or Pi disables it.
      tools: ['read', 'write', 'edit', 'bash', 'grep', 'find', 'ls', 'check_protected_api'],
      customTools: [protectedTool],
    });

    // Forward Pi's events to the platform as the neutral vocabulary.
    instrumentPiSession(session, registryUrl, agentId);

    // Capture the final assistant text by accumulating streamed deltas, resetting
    // at each turn so only the last turn's natural-language answer remains.
    const capture = { text: '' };
    session.subscribe((event: any) => {
      if (event?.type === 'turn_start') {
        capture.text = '';
      } else if (
        event?.type === 'message_update' &&
        event.assistantMessageEvent?.type === 'text_delta'
      ) {
        capture.text += event.assistantMessageEvent.delta ?? '';
      }
    });

    return { session, capture };
  })();

  sessions.set(sessionId, created);
  return created;
}

// Serialize prompts per session: a Pi AgentSession processes one prompt at a
// time, and the capture buffer is shared, so concurrent chats on the same
// session must queue.
const chains = new Map<string, Promise<unknown>>();
function runExclusive<T>(key: string, fn: () => Promise<T>): Promise<T> {
  const prev = chains.get(key) ?? Promise.resolve();
  const next = prev.catch(() => {}).then(fn);
  chains.set(key, next.catch(() => {}));
  return next;
}

async function handlePrompt(sessionId: string, message: string): Promise<string> {
  return runExclusive(sessionId, async () => {
    const { session, capture } = await getSession(sessionId);
    capture.text = '';

    // prompt() has no signal; enforce the deadline by aborting the turn.
    const timer = setTimeout(() => {
      void session.abort().catch(() => {});
    }, promptTimeoutMs);

    try {
      await session.prompt(message);
    } finally {
      clearTimeout(timer);
    }
    return capture.text.trim();
  });
}

// --- HTTP server: the platform's chat ingress contract ----------------------
const app = express();
app.use(express.json());

app.get('/health', (_req, res) => {
  res.sendStatus(200);
});

app.post('/agents/chat/:sessionId', async (req, res) => {
  const sessionId = req.params.sessionId || 'default';
  const message = String(req.body?.message ?? '').trim();

  if (!message) {
    res.json({
      response:
        'I am a Pi coding agent. Ask me to write or run code, or to "check the protected API" to exercise my Spawnly identity.',
      agentId,
      timestamp: new Date().toISOString(),
    });
    return;
  }

  try {
    const text = await handlePrompt(sessionId, message);
    res.json({
      response: text || '(the agent finished without a text reply)',
      agentId,
      timestamp: new Date().toISOString(),
    });
  } catch (err) {
    // Drop the session so the next message starts clean rather than reusing a
    // potentially wedged one.
    sessions.delete(sessionId);
    const detail = err instanceof Error ? err.message : String(err);
    await postEvent(registryUrl, agentId, 'agent_error', { error: detail });
    res.json({
      response: `Sorry — something went wrong handling that: ${detail}`,
      agentId,
      timestamp: new Date().toISOString(),
    });
  }
});

const PORT = 8080;
app.listen(PORT, () => {
  console.log(`[pi-worker] ${agentId} listening on :${PORT}`);
});
