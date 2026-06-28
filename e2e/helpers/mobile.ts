import { createHash, randomBytes } from 'node:crypto';
import { type Page, type APIResponse } from '@playwright/test';

// The mobile-gateway's public surface, port-forwarded by scripts/e2e.sh
// (svc/mobile-gateway 8080 → localhost:8091). Override with GATEWAY_URL.
export const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8091';

function base64url(b: Buffer): string {
  return b.toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// mobileAccessToken mints a user access token the way the native app does: the
// public `mobile` IdP client's authorization-code + PKCE flow. It reuses the
// browser's existing IdentityServer session (the suite logs in as alice once),
// so the /connect/authorize call redirects straight to the app's redirect URI
// with a code — no second login. The dashboard origin proxies /connect/* to the
// IdP (single-issuer), so everything goes through the dashboard baseURL.
export async function mobileAccessToken(page: Page): Promise<string> {
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash('sha256').update(verifier).digest());
  const redirectUri = 'spawnly://auth';
  const scope = 'openid orchestrator:read orchestrator:write offline_access';

  const authorize = await page.request.get('/connect/authorize', {
    params: {
      client_id: 'mobile',
      response_type: 'code',
      redirect_uri: redirectUri,
      scope,
      code_challenge: challenge,
      code_challenge_method: 'S256',
      state: base64url(randomBytes(8)),
    },
    // Capture the redirect to spawnly://auth (an unfollowable custom scheme)
    // rather than letting the client choke trying to follow it.
    maxRedirects: 0,
  });
  const location = authorize.headers()['location'];
  if (!location || !location.startsWith(redirectUri)) {
    throw new Error(`authorize did not redirect to the app (status ${authorize.status()}, location ${location}); is the alice session present and the mobile client configured?`);
  }
  const code = new URL(location).searchParams.get('code');
  if (!code) throw new Error(`no authorization code in ${location}`);

  const token = await page.request.post('/connect/token', {
    form: {
      grant_type: 'authorization_code',
      code,
      redirect_uri: redirectUri,
      client_id: 'mobile',
      code_verifier: verifier,
    },
  });
  if (!token.ok()) throw new Error(`token exchange failed: ${token.status()} ${await token.text()}`);
  const body = await token.json();
  if (!body.access_token) throw new Error('token response missing access_token');
  return body.access_token as string;
}

// gw issues an authenticated request to the gateway with the user's bearer.
export function gw(page: Page, token: string) {
  const auth = { Authorization: `Bearer ${token}` };
  return {
    get: (path: string): Promise<APIResponse> =>
      page.request.get(`${GATEWAY_URL}${path}`, { headers: auth }),
    post: (path: string, data?: unknown): Promise<APIResponse> =>
      page.request.post(`${GATEWAY_URL}${path}`, { headers: auth, data: data ?? {} }),
  };
}

export interface GatewayConsentRequest {
  id: string;
  agentId?: string;
  parentType: string;
  childType: string;
  scopes?: string[];
}

// streamFirstEvent opens the SSE stream and resolves with the first `data:`
// frame's parsed JSON whose consentRequestId is present, or rejects on timeout.
// Uses fetch (Node 18+) so we can read the body incrementally.
export async function streamFirstConsentEvent(
  token: string,
  timeoutMs = 120_000,
): Promise<{ consentRequestId: string; childType: string }> {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const resp = await fetch(`${GATEWAY_URL}/me/stream`, {
      headers: { Authorization: `Bearer ${token}` },
      signal: ctrl.signal,
    });
    if (!resp.ok || !resp.body) throw new Error(`stream open failed: ${resp.status}`);
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    for (;;) {
      const { value, done } = await reader.read();
      if (done) throw new Error('stream closed before an event arrived');
      buf += decoder.decode(value, { stream: true });
      let nl: number;
      while ((nl = buf.indexOf('\n')) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (line.startsWith('data: ')) {
          const e = JSON.parse(line.slice('data: '.length));
          if (e.consentRequestId) return e;
        }
      }
    }
  } finally {
    clearTimeout(timer);
    ctrl.abort();
  }
}
