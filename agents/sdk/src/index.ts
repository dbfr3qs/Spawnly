interface TokenResponse {
  access_token: string;
  expires_in: number;
}

interface CachedToken {
  token: string;
  expiresAt: number;
}

const EXPIRY_BUFFER_MS = 5_000;

export class TokenClient {
  private baseUrl: string;
  private cache = new Map<string, CachedToken>();

  constructor(baseUrl = "http://localhost:8089") {
    this.baseUrl = baseUrl;
  }

  async getToken(scope: string): Promise<string> {
    const cached = this.cache.get(scope);
    if (cached && Date.now() < cached.expiresAt - EXPIRY_BUFFER_MS) {
      return cached.token;
    }

    const res = await fetch(`${this.baseUrl}/token?scope=${encodeURIComponent(scope)}`);
    if (!res.ok) {
      throw new Error(`Token request failed: ${res.status} ${res.statusText}`);
    }

    const data = (await res.json()) as TokenResponse;
    this.cache.set(scope, {
      token: data.access_token,
      expiresAt: Date.now() + data.expires_in * 1_000,
    });

    return data.access_token;
  }
}

type FetchSignature = typeof fetch;

export function createAuthenticatedFetch(
  baseUrl: string,
  scope: string,
  tokenClient: TokenClient = new TokenClient()
): FetchSignature {
  return async (input: RequestInfo | URL, init?: RequestInit) => {
    const token = await tokenClient.getToken(scope);

    const headers = new Headers(init?.headers);
    headers.set("Authorization", `Bearer ${token}`);

    const url =
      typeof input === "string" && !input.startsWith("http")
        ? `${baseUrl}${input}`
        : input;

    return fetch(url, { ...init, headers });
  };
}

export async function postEvent(
  registryUrl: string,
  agentId: string,
  type: string,
  payload: unknown
): Promise<void> {
  try {
    await fetch(`${registryUrl}/v1/agents/${agentId}/events`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ source: 'agent', type, payload }),
    });
  } catch (e: unknown) {
    console.warn(`[sdk] postEvent failed: ${e}`);
  }
}

// --- Flue instrumentation ----------------------------------------------------
// Minimal structural type so the SDK stays dependency-free and does NOT import
// from @flue/runtime. Any object exposing subscribeEvent satisfies this.
type EventSubscribable = { subscribeEvent(cb: (event: any) => void): () => void };

const TRUNCATE_LIMIT = 2_000;

// Best-effort, never-throwing serialization capped at ~2000 chars.
function truncate(v: unknown): string {
  let s: string;
  try {
    s = JSON.stringify(v);
    if (s === undefined) {
      s = String(v);
    }
  } catch {
    try {
      s = String(v);
    } catch {
      s = '[unserializable]';
    }
  }
  if (s.length > TRUNCATE_LIMIT) {
    return s.slice(0, TRUNCATE_LIMIT) + '…[truncated]';
  }
  return s;
}

/**
 * Tap a Flue runtime context's in-process event stream and forward a NEUTRAL,
 * framework-agnostic event vocabulary into the platform's event pipeline via
 * postEvent. Never throws from the subscription callback. Returns the
 * unsubscribe function from subscribeEvent.
 */
export function instrumentFlue(
  ctx: EventSubscribable,
  registryUrl: string,
  agentId: string
): () => void {
  return ctx.subscribeEvent((event: any) => {
    try {
      const type: string = event?.type;
      if (type === undefined || type === null) {
        return;
      }

      // Drop high-frequency noise.
      if (type === 'text_delta' || type === 'thinking_delta') {
        return;
      }

      switch (type) {
        case 'run_start':
          void postEvent(registryUrl, agentId, 'run_start', {
            agentName: event.agentName,
            startedAt: event.startedAt,
          });
          return;
        case 'run_end':
          void postEvent(registryUrl, agentId, 'run_end', {
            isError: event.isError,
            durationMs: event.durationMs,
            error: truncate(event.error),
          });
          return;
        case 'turn':
          void postEvent(registryUrl, agentId, 'model_turn', {
            model: event.model,
            stopReason: event.stopReason,
            durationMs: event.durationMs,
            isError: event.isError,
            usage: event.usage,
          });
          return;
        case 'tool_start':
          void postEvent(registryUrl, agentId, 'tool_start', {
            toolName: event.toolName,
            toolCallId: event.toolCallId,
            args: truncate(event.args),
          });
          return;
        case 'tool_call':
          void postEvent(registryUrl, agentId, 'tool_end', {
            toolName: event.toolName,
            toolCallId: event.toolCallId,
            isError: event.isError,
            durationMs: event.durationMs,
            result: truncate(event.result),
          });
          return;
        case 'thinking_start':
          void postEvent(registryUrl, agentId, 'thinking', { phase: 'start' });
          return;
        case 'thinking_end':
          void postEvent(registryUrl, agentId, 'thinking', { phase: 'end' });
          return;
        case 'compaction_start':
          void postEvent(registryUrl, agentId, 'compaction', {
            phase: 'start',
            reason: event.reason,
            estimatedTokens: event.estimatedTokens,
          });
          return;
        case 'compaction':
          void postEvent(registryUrl, agentId, 'compaction', {
            phase: 'end',
            durationMs: event.durationMs,
          });
          return;
        case 'internal_error':
        case 'gave_up':
          void postEvent(registryUrl, agentId, 'error', {
            kind: type,
            detail: truncate(event),
          });
          return;
        case 'log':
          void postEvent(registryUrl, agentId, 'log', {
            level: event.level,
            message: event.message,
          });
          return;
        default:
          // Any other non-delta type (idle, operation_start, operation,
          // task_start, task, ...): the Flue type name is already a neutral
          // activity label, so forward it as-is with a small payload.
          void postEvent(registryUrl, agentId, type, truncate(event));
          return;
      }
    } catch (e: unknown) {
      // Must never break the agent.
      console.warn(`[sdk] instrumentFlue handler failed: ${e}`);
    }
  });
}

/** Returns an AbortSignal that aborts after `ms` milliseconds. */
export function promptTimeoutSignal(ms: number): AbortSignal {
  return AbortSignal.timeout(ms);
}
