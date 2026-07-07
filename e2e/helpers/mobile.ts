import { createHash, randomBytes } from 'node:crypto';
import { type Page, type APIResponse } from '@playwright/test';
import { DEMO_USER, DEMO_PASS } from './auth';

// The mobile-gateway's public surface, port-forwarded by scripts/e2e.sh
// (svc/mobile-gateway 8080 → localhost:8091). Override with GATEWAY_URL.
export const GATEWAY_URL = process.env.GATEWAY_URL ?? 'http://localhost:8091';

function base64url(b: Buffer): string {
  return b.toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// mobileAccessToken mints a user access token the way the native app does: the
// public `mobile` IdP client's authorization-code + PKCE flow. It performs a
// real alice login against Keycloak (rather than relying on browser SSO reuse,
// which is unreliable on kind: Keycloak's SSO cookies are Secure+SameSite=None
// and neither Playwright's page.request nor a page navigation reliably sends
// them over plain http://localhost). The whole flow runs over Node fetch — with
// full control of the auth-flow cookies — through the dashboard origin, which
// proxies Keycloak's /realms/* endpoints single-origin. NOTE: scope carries
// spawnly-roles (not `roles`) so the flat `role` claim rides, matching
// mobile/src/config.ts. The caller navigates to '/' first, so page.url() yields
// the dashboard origin.
export async function mobileAccessToken(page: Page): Promise<string> {
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash('sha256').update(verifier).digest());
  const redirectUri = 'spawnly://auth';
  const scope = 'openid spawnly-roles orchestrator:read orchestrator:write offline_access';

  const params = new URLSearchParams({
    client_id: 'mobile',
    response_type: 'code',
    redirect_uri: redirectUri,
    scope,
    code_challenge: challenge,
    code_challenge_method: 'S256',
    state: base64url(randomBytes(8)),
  });

  const origin = new URL(page.url()).origin;

  // 1. GET authorize → Keycloak renders the login page (200) and sets the
  //    transient auth-flow cookies (AUTH_SESSION_ID etc.). Grab the login form's
  //    POST action and carry those cookies to the credential POST.
  const authPage = await fetch(
    `${origin}/realms/spawnly/protocol/openid-connect/auth?${params.toString()}`,
    { redirect: 'manual' },
  );
  // Cookie jar carried across the auth-flow requests (accumulates AUTH_SESSION_ID
  // etc.; later steps refresh them).
  const jar = new Map<string, string>();
  const stash = (resp: Response) => {
    for (const c of resp.headers.getSetCookie?.() ?? []) {
      const pair = c.split(';')[0];
      const eq = pair.indexOf('=');
      if (eq > 0) jar.set(pair.slice(0, eq).trim(), pair.slice(eq + 1).trim());
    }
  };
  const cookieHeader = () => [...jar].map(([k, v]) => `${k}=${v}`).join('; ');
  const formAction = (html: string) =>
    html.match(/id="kc-form-login"[\s\S]*?action="([^"]+)"/)?.[1]?.replace(/&amp;/g, '&');

  let location = authPage.headers.get('location');
  if (!(location && location.startsWith(redirectUri))) {
    // Not already SSO'd → drive the login form(s). With realm Organizations
    // enabled the flow is IDENTITY-FIRST: username on the first screen, password
    // on a second (so Keycloak can broker an org's IdP before asking for a
    // password). We walk each kc-form-login page, POSTing whichever fields it
    // asks for and carrying the flow cookies forward, until Keycloak 302s to the
    // app's custom-scheme redirect (spawnly://auth?code=…). A single-screen form
    // (both fields at once) completes on the first iteration.
    stash(authPage);
    let html = await authPage.text();
    for (let step = 0; step < 3; step++) {
      const action = formAction(html);
      if (!action) throw new Error('mobile: could not find the Keycloak login form action');
      const form = new URLSearchParams();
      if (/name="username"/.test(html)) form.set('username', DEMO_USER);
      if (/name="password"/.test(html) || /type="password"/.test(html)) form.set('password', DEMO_PASS);

      // redirect:'manual' captures the custom-scheme Location without following it.
      let resp = await fetch(action, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', Cookie: cookieHeader() },
        body: form.toString(),
        redirect: 'manual',
      });
      stash(resp);
      location = resp.headers.get('location');
      if (location && location.startsWith(redirectUri)) break;

      // Not at the app yet → advance to the next screen. A 302 points at the
      // next auth-flow GET (the identity-first password screen); a 200 already
      // IS the next form. Fetch the next form's HTML either way.
      if (location) {
        resp = await fetch(location, { headers: { Cookie: cookieHeader() }, redirect: 'manual' });
        stash(resp);
        location = resp.headers.get('location');
        if (location && location.startsWith(redirectUri)) break;
      }
      html = await resp.text();
    }
  }
  if (!location || !location.startsWith(redirectUri)) {
    throw new Error(`mobile login did not redirect to the app (location ${location}); is the mobile client configured?`);
  }
  const code = new URL(location).searchParams.get('code');
  if (!code) throw new Error(`no authorization code in ${location}`);

  const tokenResp = await fetch(`${origin}/realms/spawnly/protocol/openid-connect/token`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
    body: new URLSearchParams({
      grant_type: 'authorization_code',
      code,
      redirect_uri: redirectUri,
      client_id: 'mobile',
      code_verifier: verifier,
    }).toString(),
  });
  if (!tokenResp.ok) throw new Error(`token exchange failed: ${tokenResp.status} ${await tokenResp.text()}`);
  const body = await tokenResp.json();
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
