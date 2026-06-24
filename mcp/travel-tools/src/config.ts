// Runtime configuration for the travel-tools server and its upstream adapters.
// Provider API keys live ONLY here (read from the travel-tools-secrets env); they
// are never exposed to agents. URLs are overridable so tests can point adapters
// at a stubbed fetch, and `fetch` is injectable for the same reason.

export interface ToolConfig {
  frankfurterUrl: string;
  duffel: { url: string; apiKey?: string; version: string };
  liteapi: { url: string; apiKey?: string };
  // Injectable for tests; defaults to the global fetch in production.
  fetch: typeof fetch;
}

export function configFromEnv(env: NodeJS.ProcessEnv = process.env): ToolConfig {
  return {
    frankfurterUrl: env.FRANKFURTER_URL ?? "https://api.frankfurter.dev/v1",
    duffel: {
      url: env.DUFFEL_URL ?? "https://api.duffel.com",
      apiKey: env.DUFFEL_API_KEY || undefined,
      version: env.DUFFEL_VERSION ?? "v2",
    },
    liteapi: {
      url: env.LITEAPI_URL ?? "https://api.liteapi.travel/v3.0",
      apiKey: env.LITEAPI_KEY || undefined,
    },
    fetch: globalThis.fetch,
  };
}
