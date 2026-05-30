interface TokenResponse {
  access_token: string;
  expires_in: number;
}

interface CachedToken {
  token: string;
  expiresAt: number;
}

const EXPIRY_BUFFER_MS = 5_000;

export interface TokenClientOptions {
  /** Deadline for the sidecar-not-ready retry loop. Default 30s. */
  readyTimeoutMs?: number;
  /** Backoff between retries while the sidecar is unreachable. Default 1s. */
  retryDelayMs?: number;
}

export interface TokenRequestOptions {
  /** Abort the (possibly retrying) request early. */
  signal?: AbortSignal;
}

/**
 * Wraps the per-agent sidecar's `/token` endpoint — the platform's neutral token
 * contract. Covers all three modes the sidecar exposes:
 *   - getToken(scope)                      → client_credentials
 *   - getToken(scope, { audience })        → client_credentials, explicit audience
 *                                            (e.g. minting a delegation token)
 *   - exchangeToken({ subjectToken, ... }) → RFC 8693 token-exchange
 *
 * The sidecar binds :8089 only after it has fetched its SVID and self-registered,
 * so the first calls at startup can hit ECONNREFUSED. Requests retry on
 * connection errors / 5xx until `readyTimeoutMs`, but fail fast on 4xx (a bad
 * scope or policy denial is not a readiness problem).
 */
export class TokenClient {
  private baseUrl: string;
  private readyTimeoutMs: number;
  private retryDelayMs: number;
  private cache = new Map<string, CachedToken>();

  constructor(baseUrl = "http://localhost:8089", opts: TokenClientOptions = {}) {
    this.baseUrl = baseUrl;
    this.readyTimeoutMs = opts.readyTimeoutMs ?? 30_000;
    this.retryDelayMs = opts.retryDelayMs ?? 1_000;
  }

  /**
   * Client-credentials token for `scope`. Cached per `scope|audience` until just
   * before expiry. Pass `audience` to target a specific resource / mint a
   * delegation token (e.g. `{ audience: "delegation" }`).
   */
  async getToken(scope: string, opts: TokenRequestOptions & { audience?: string } = {}): Promise<string> {
    const { audience, signal } = opts;
    const cacheKey = `${scope}|${audience ?? ""}`;
    const cached = this.cache.get(cacheKey);
    if (cached && Date.now() < cached.expiresAt - EXPIRY_BUFFER_MS) {
      return cached.token;
    }

    const params: Record<string, string> = { scope };
    if (audience) params.audience = audience;
    const data = await this.request(params, signal);

    this.cache.set(cacheKey, {
      token: data.access_token,
      expiresAt: Date.now() + data.expires_in * 1_000,
    });
    return data.access_token;
  }

  /**
   * RFC 8693 token-exchange: exchange a `subjectToken` (e.g. a delegation token
   * received from a parent) for a token scoped to `audience`/`scope`, with this
   * agent's SVID added to the act chain by the sidecar. Never cached — exchanged
   * tokens are short-lived and request-specific.
   */
  async exchangeToken(
    args: { subjectToken: string; audience: string; scope: string },
    opts: TokenRequestOptions = {},
  ): Promise<string> {
    const data = await this.request(
      { subject_token: args.subjectToken, audience: args.audience, scope: args.scope },
      opts.signal,
    );
    return data.access_token;
  }

  // Issues the GET, retrying on connection errors / 5xx until the readiness
  // deadline, failing fast on any 4xx. Honors an optional AbortSignal.
  private async request(params: Record<string, string>, signal?: AbortSignal): Promise<TokenResponse> {
    const qs = new URLSearchParams(params).toString();
    const url = `${this.baseUrl}/token?${qs}`;
    const deadline = Date.now() + this.readyTimeoutMs;
    let lastErr: unknown;

    while (Date.now() < deadline) {
      if (signal?.aborted) throw new Error("token request aborted");

      let res: Response;
      try {
        res = await fetch(url, { signal });
      } catch (e) {
        // Connection error (sidecar not listening yet) or abort.
        if (signal?.aborted) throw e;
        lastErr = e;
        await this.sleep(signal);
        continue;
      }

      if (res.ok) {
        return (await res.json()) as TokenResponse;
      }
      // 4xx — a real error (bad scope / policy denial), not a readiness issue.
      if (res.status >= 400 && res.status < 500) {
        throw new Error(`token request failed: ${res.status} ${await res.text()}`);
      }
      // 5xx — transient; retry until the deadline.
      lastErr = new Error(`token request failed: ${res.status}`);
      await this.sleep(signal);
    }
    throw new Error(`sidecar /token unreachable after ${this.readyTimeoutMs}ms: ${lastErr}`);
  }

  private sleep(signal?: AbortSignal): Promise<void> {
    return new Promise((resolve, reject) => {
      const id = setTimeout(resolve, this.retryDelayMs);
      signal?.addEventListener(
        "abort",
        () => {
          clearTimeout(id);
          reject(new Error("token request aborted"));
        },
        { once: true },
      );
    });
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
