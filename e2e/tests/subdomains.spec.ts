import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import {
  APEX_HOST,
  APEX_ORIGIN,
  loginAt,
  orgOrigin,
  LOGIN_USERNAME,
  LOGIN_PASSWORD,
  LOGIN_SUBMIT,
} from '../helpers/auth';
import { spawn, type AgentSummary } from '../helpers/dashboard';

// ─────────────────────────────────────────────────────────────────────────────
// Scenario — organization SUBDOMAINS, proven end-to-end over the Phase-3 local
// TLS mimic (make tls-mimic → https://<slug>.localtest.me). This suite runs ONLY
// under SUBDOMAIN_E2E=1 (the `subdomains` Playwright project) against the apex
// baseURL https://localtest.me; the default `make e2e` never picks it up.
//
// Seed matrix (deploy/keycloak/users.tf + scripts/reseed-orgs.sh):
//   acme   = alice (org admin) + viewer (member) + carol (member)
//   globex = bob   (org admin) + carol (member)
//   nomad  = member of NO org
// carol is the two-org fixture that exists ONLY to prove silent cross-subdomain
// SSO (case b); she is seeded LAST so she never perturbs the single-org matrix.
//
// The six spec cases (plans/org-subdomains/spec.md §Testing & QA):
//   (a) fresh satellite login via the apex auth origin → satellite-scoped session
//   (b) silent cross-subdomain SSO for a two-org user
//   (c) self-heal — a member stays scoped; a non-member is denied (never scoped)
//   (d) security — a held satellite session cannot reach another org's host, and
//       the orchestrator still enforces org from the TOKEN, not the Host
//   (e) per-origin session cookie isolation (no Domain=.localtest.me)
//   (f) the apex still serves Global/admin + org picker + signup + the KC proxy
//
// NOTE ON ORIGINS: the project baseURL is the APEX, but every request in this
// suite targets a specific origin (apex or a satellite). `page.request` resolves
// relative URLs against baseURL, so we ALWAYS use ABSOLUTE origins here — the
// per-origin session cookie is the whole point. Never use the relative-path
// helpers from dashboard.ts in this file.

// GET <origin>/api/me for the page's context (sends that origin's cookies only).
async function apiMe(page: Page, origin: string): Promise<Record<string, any>> {
  const r = await page.request.get(`${origin}/api/me`);
  expect(r.ok(), `GET ${origin}/api/me: ${r.status()}`).toBeTruthy();
  return r.json();
}

// GET <origin>/api/agents; returns [] when the call is not OK (e.g. a 401 for an
// origin whose per-origin cookie the browser refused to send).
async function agentsAt(page: Page, origin: string): Promise<AgentSummary[]> {
  const r = await page.request.get(`${origin}/api/agents`);
  if (!r.ok()) return [];
  const body = await r.json();
  return Array.isArray(body) ? body : [];
}

function hostOf(url: string): string {
  return new URL(url).host;
}

// Drive the identity-first Keycloak login WITHOUT asserting a dashboard landing —
// for the non-member / gated flows (bob at acme, carol/nomad at the apex picker)
// where the flow deliberately does NOT end on #agent-list. Starts at `origin`.
async function submitLogin(page: Page, origin: string, user: string, pass: string) {
  await page.goto(`${origin}/`);
  await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
  await page.locator(LOGIN_USERNAME).fill(user);
  const password = page.locator(LOGIN_PASSWORD);
  if (!(await password.isVisible())) {
    await page.locator(LOGIN_SUBMIT).click();
    await expect(password).toBeVisible({ timeout: 30_000 });
  }
  await password.fill(pass);
  await page.locator(LOGIN_SUBMIT).click();
}

test.describe('organization subdomains', () => {
  let aliceCtx: BrowserContext;
  let alice: Page;
  let acmeAgentId = '';

  test.beforeAll(async ({ browser }) => {
    // alice (acme admin) logs in at the acme satellite and spawns one acme-owned
    // agent that the isolation cases (a/c/d) assert against. Wait for `active` so
    // its SpiceDB owner/org tuples are written (visibility depends on them).
    aliceCtx = await browser.newContext();
    alice = await aliceCtx.newPage();
    await loginAt(alice, orgOrigin('acme'), 'alice', 'alice');
    acmeAgentId = await spawn(alice, 'weather-monitor');
    await expect
      .poll(
        async () => (await agentsAt(alice, orgOrigin('acme'))).find((a) => a.agentId === acmeAgentId)?.status ?? '',
        { timeout: 120_000 },
      )
      .toMatch(/active/i);
  });

  test.afterAll(async () => {
    // Kill the agent through the acme origin (alice's own session cookie).
    if (acmeAgentId && alice) {
      await alice.request.delete(`${orgOrigin('acme')}/api/agents/${acmeAgentId}`).catch(() => {});
    }
    await aliceCtx?.close();
  });

  test('(a) a fresh satellite visit logs in via the APEX auth origin and lands scoped to the org', async ({
    browser,
  }) => {
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      // A fresh (no-SSO) visit to the acme satellite is bounced to Keycloak, which
      // is proxied ONLY by the apex — so the login form must be served from the
      // apex host, never the satellite (single OIDC issuer / auth origin).
      await page.goto(`${orgOrigin('acme')}/`);
      await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
      expect(hostOf(page.url()), 'the login form must be served from the apex auth origin').toBe(APEX_HOST);

      // Complete the login as an acme member; the redirect chain returns to the
      // satellite's own /callback and the scoped shell.
      await page.locator(LOGIN_USERNAME).fill('viewer');
      const pw = page.locator(LOGIN_PASSWORD);
      if (!(await pw.isVisible())) {
        await page.locator(LOGIN_SUBMIT).click();
        await expect(pw).toBeVisible({ timeout: 30_000 });
      }
      await pw.fill('viewer');
      await page.locator(LOGIN_SUBMIT).click();
      await expect(page.locator('#agent-list')).toBeVisible({ timeout: 30_000 });

      // Back on the satellite host, scoped to acme.
      expect(hostOf(page.url()), 'lands back on the satellite').toBe(`acme.${APEX_HOST}`);
      const me = await apiMe(page, orgOrigin('acme'));
      expect(me.activeOrg, 'session is scoped to acme').toBe('acme');
      expect(me.hostOrg, 'host resolves to acme').toBe('acme');
      expect(me.isApex, 'a satellite is not the apex').toBe(false);

      // The scoped list shows the acme agent (member visibility) — a real proof the
      // satellite session reaches acme's data.
      await expect
        .poll(async () => (await agentsAt(page, orgOrigin('acme'))).some((a) => a.agentId === acmeAgentId), {
          timeout: 30_000,
        })
        .toBe(true);
    } finally {
      await ctx.close();
    }
  });

  test('(b) a two-org member is logged into their second org SILENTLY across subdomains', async ({
    browser,
  }) => {
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      // carol logs in at acme (establishing the shared apex SSO session)...
      await loginAt(page, orgOrigin('acme'), 'carol', 'carol');
      expect((await apiMe(page, orgOrigin('acme'))).activeOrg).toBe('acme');

      // ...then visits her OTHER org's subdomain. The host↔claim mismatch self-heals
      // via a SILENT (prompt=none) re-auth on the apex SSO cookie — no login form may
      // ever appear (same guarantee as the in-app /switch-org, orgs.spec.ts).
      await page.goto(`${orgOrigin('globex')}/`);
      const list = page.locator('#agent-list');
      const uname = page.locator(LOGIN_USERNAME);
      await expect(list.or(uname).first()).toBeVisible({ timeout: 30_000 });
      await expect(uname, 'silent cross-subdomain SSO must be password-less (no login form)').not.toBeVisible();
      await expect(list).toBeVisible();

      expect(hostOf(page.url())).toBe(`globex.${APEX_HOST}`);
      await expect
        .poll(async () => (await apiMe(page, orgOrigin('globex'))).activeOrg, { timeout: 30_000 })
        .toBe('globex');
    } finally {
      await ctx.close();
    }
  });

  test('(c) self-heal: a member on their own host stays scoped; a non-member is denied and never scoped', async ({
    browser,
  }) => {
    // Member on a consistent host: viewer (acme member) at the acme satellite
    // resolves cleanly to acme and stays put — the positive self-heal outcome.
    const memberCtx = await browser.newContext();
    try {
      const page = await memberCtx.newPage();
      await loginAt(page, orgOrigin('acme'), 'viewer', 'viewer');
      expect((await apiMe(page, orgOrigin('acme'))).activeOrg, 'member stays scoped to their host org').toBe('acme');
    } finally {
      await memberCtx.close();
    }

    // Non-member: bob (globex admin, NOT an acme member) at the acme satellite. The
    // satellite starts the login with the `organization=acme` authorize param, so
    // Keycloak's org-scoped flow DENIES a non-member at the IdP — after entering his
    // credentials he is bounced back to the apex login and never obtains ANY acme
    // session. (This is a STRONGER denial than the app-level not-a-member screen the
    // plan anticipated: the non-member never even gets an org-less dashboard session
    // on the satellite.) Either way, he NEVER reaches an acme-scoped view.
    const nonMemberCtx = await browser.newContext();
    try {
      const page = await nonMemberCtx.newPage();
      await submitLogin(page, orgOrigin('acme'), 'bob', 'bob');

      // He never lands on a working dashboard: #agent-list must not appear, and he
      // holds NO acme session (the acme /api/me is 401 — unauthenticated). Assert it
      // STAYS denied over a window, so a late redirect can't slip him into acme.
      await expect
        .poll(async () => (await page.request.get(`${orgOrigin('acme')}/api/me`)).status(), { timeout: 20_000 })
        .toBe(401);
      await expect(page.locator('#agent-list'), 'a non-member never reaches an acme dashboard').not.toBeVisible();
      // The denial happened at the apex auth origin (the org-scoped login), not on
      // the satellite — he is still parked on the Keycloak login served by the apex.
      expect(hostOf(page.url()), 'a denied non-member is bounced back to the apex login').toBe(APEX_HOST);
    } finally {
      await nonMemberCtx.close();
    }
  });

  test('(d) security: a held satellite session cannot reach another org, and the orchestrator enforces from the TOKEN not the Host', async ({
    browser,
  }) => {
    // viewer holds a live acme satellite session.
    const viewerCtx = await browser.newContext();
    // bob holds a live globex satellite session.
    const bobCtx = await browser.newContext();
    try {
      const viewer = await viewerCtx.newPage();
      await loginAt(viewer, orgOrigin('acme'), 'viewer', 'viewer');
      // Token-scoped read works on the matching host: acme session sees acme's agent.
      expect((await agentsAt(viewer, orgOrigin('acme'))).some((a) => a.agentId === acmeAgentId)).toBe(true);

      // Replay that SAME context against the globex host. The acme session cookie is
      // host-scoped (acme.localtest.me), so the browser refuses to send it to globex:
      // the request is unauthenticated (401) and returns NO globex/acme data. Host
      // alone grants nothing — browser containment, not authorization.
      const crossHost = await viewer.request.get(`${orgOrigin('globex')}/api/agents`);
      expect(crossHost.status(), 'acme cookie must not authenticate on the globex host').toBe(401);
      expect((await agentsAt(viewer, orgOrigin('globex'))).some((a) => a.agentId === acmeAgentId)).toBe(false);

      // Belt-and-suspenders (acceptance criterion 11): the orchestrator derives org
      // from the TOKEN, independent of Host. bob's globex session — a legitimate
      // authenticated caller on a DIFFERENT org host — never sees the acme agent,
      // because his token carries globex, not because of any host check.
      const bob = await bobCtx.newPage();
      await loginAt(bob, orgOrigin('globex'), 'bob', 'bob');
      expect((await agentsAt(bob, orgOrigin('globex'))).some((a) => a.agentId === acmeAgentId)).toBe(false);
    } finally {
      await viewerCtx.close();
      await bobCtx.close();
    }
  });

  test('(e) session cookies are per-origin — no shared Domain=.localtest.me', async ({ browser }) => {
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      await loginAt(page, orgOrigin('acme'), 'viewer', 'viewer');

      // The dash_session cookie exists for the acme satellite and is HOST-scoped to
      // exactly acme.localtest.me (Playwright reports a host-only cookie's domain
      // WITHOUT a leading dot; a Domain=.localtest.me cookie would report
      // ".localtest.me" and be sent to every subdomain).
      const acmeCookies = await ctx.cookies(orgOrigin('acme'));
      const sess = acmeCookies.find((c) => c.name === 'dash_session');
      expect(sess, 'acme satellite must have its own dash_session cookie').toBeTruthy();
      expect(sess!.domain, 'the session cookie must be host-scoped, not a shared parent-domain cookie').toBe(
        `acme.${APEX_HOST}`,
      );

      // It is absent on the apex and on other satellites (the browser would only
      // surface it there if it were a Domain=.localtest.me cookie).
      const apexCookies = await ctx.cookies(APEX_ORIGIN);
      expect(apexCookies.some((c) => c.name === 'dash_session'), 'no acme session cookie on the apex').toBe(false);
      const globexCookies = await ctx.cookies(orgOrigin('globex'));
      expect(globexCookies.some((c) => c.name === 'dash_session'), 'no acme session cookie on another satellite').toBe(
        false,
      );
    } finally {
      await ctx.close();
    }
  });

  test('(f) the apex serves Global/admin and proxies Keycloak; a satellite does not proxy Keycloak', async ({
    browser,
  }) => {
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      // alice (realm admin) at the apex lands on the org-less Global view.
      await loginAt(page, APEX_ORIGIN, 'alice', 'alice');
      expect(hostOf(page.url())).toBe(APEX_HOST);
      await expect(page.locator('[data-testid="active-org"]')).toContainText(/global/i, { timeout: 30_000 });
      const me = await apiMe(page, APEX_ORIGIN);
      expect(me.isApex, 'the apex is the apex').toBe(true);
      expect(me.activeOrg, 'the Global view is org-less').toBeFalsy();
      expect(me.isAdmin, 'alice is a realm admin').toBe(true);

      // The apex proxies Keycloak (single OIDC issuer lives here)...
      const apexKc = await page.request.get(`${APEX_ORIGIN}/realms/spawnly/.well-known/openid-configuration`);
      expect(apexKc.status(), 'the apex proxies Keycloak').toBe(200);
      // ...but a satellite NEVER does — the KC-proxy path 404s (criterion 6).
      const satKc = await page.request.get(`${orgOrigin('acme')}/realms/spawnly/.well-known/openid-configuration`);
      expect(satKc.status(), 'a satellite must not proxy Keycloak').toBe(404);
    } finally {
      await ctx.close();
    }
  });

  test('(f) the apex serves the org picker (multi-org) and self-serve signup (org-less)', async ({ browser }) => {
    // A multi-org user landing org-less on the apex gets the PICKER.
    const carolCtx = await browser.newContext();
    try {
      const page = await carolCtx.newPage();
      await submitLogin(page, APEX_ORIGIN, 'carol', 'carol');
      await expect(page.locator('[data-testid="org-picker"]'), 'apex shows the org picker for a multi-org user').toBeVisible({
        timeout: 30_000,
      });
    } finally {
      await carolCtx.close();
    }

    // A user in NO org gets the no-organization screen with the self-serve signup form.
    const nomadCtx = await browser.newContext();
    try {
      const page = await nomadCtx.newPage();
      await submitLogin(page, APEX_ORIGIN, 'nomad', 'nomad');
      await expect(page.locator('[data-testid="no-org-screen"]'), 'apex shows the no-org screen').toBeVisible({
        timeout: 30_000,
      });
      await expect(page.locator('[data-testid="create-org-form"]'), 'apex serves self-serve signup').toBeVisible();
    } finally {
      await nomadCtx.close();
    }
  });

  test('(g) picking an org from the apex switcher NAVIGATES to its subdomain (silently)', async ({ browser }) => {
    // Each org's home is its subdomain, so choosing an org from the header switcher
    // must take the browser to <slug>.<apex> — from the apex too, not only from a
    // satellite. alice (realm admin) starts on the apex Global view.
    const ctx = await browser.newContext();
    try {
      const page = await ctx.newPage();
      await loginAt(page, APEX_ORIGIN, 'alice', 'alice');
      expect(hostOf(page.url()), 'starts on the apex').toBe(APEX_HOST);
      const switcher = page.locator('#org-switcher');
      await expect(switcher, 'the apex Global view offers the org switcher').toBeVisible({ timeout: 30_000 });

      // Selecting acme fires the switcher's navigate-to-subdomain: the browser goes
      // to the acme satellite and silently re-auths via the shared apex SSO session
      // (no login form). The URL must end up on the acme subdomain — the regression
      // this guards is the switch leaving the browser parked on the apex origin.
      await Promise.all([
        page.waitForURL(`https://acme.${APEX_HOST}/**`, { timeout: 30_000 }),
        switcher.selectOption('acme'),
      ]);
      await expect(page.locator(LOGIN_USERNAME), 'apex→satellite switch must be password-less').not.toBeVisible();
      await expect(page.locator('#agent-list')).toBeVisible({ timeout: 30_000 });
      expect(hostOf(page.url()), 'the switch lands on the org subdomain, not the apex').toBe(`acme.${APEX_HOST}`);
      await expect
        .poll(async () => (await apiMe(page, orgOrigin('acme'))).activeOrg, { timeout: 30_000 })
        .toBe('acme');
    } finally {
      await ctx.close();
    }
  });
});
