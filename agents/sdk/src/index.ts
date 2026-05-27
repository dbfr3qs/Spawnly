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
