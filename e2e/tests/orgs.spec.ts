import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import {
  contextFor,
  reachDashboardShell,
  LOGIN_USERNAME,
  LOGIN_PASSWORD,
  LOGIN_SUBMIT,
} from '../helpers/auth';
import { spawn, listAgents, type AgentSummary } from '../helpers/dashboard';
import { killAgent } from '../helpers/cleanup';

// switchOrg drives the header re-auth-per-org flow: navigate to /switch-org
// (org omitted = Global). The switch MUST be silent — the dashboard re-auths via
// prompt=none against the live Keycloak SSO session, so no login form may ever
// appear (that regression is exactly what dropping offline_access fixed: an
// offline_access login made Keycloak 26.1+ delete the online session, forcing a
// password on every switch). If Keycloak re-prompts, the assertion below fails
// on the login form instead of silently typing the password.
async function switchOrg(page: Page, org?: string) {
  await page.goto(org ? `/switch-org?org=${encodeURIComponent(org)}` : '/switch-org');
  const list = page.locator('#agent-list');
  const uname = page.locator(LOGIN_USERNAME);
  // Wait for the flow to settle on either outcome, then require it was silent.
  await expect(list.or(uname).first()).toBeVisible({ timeout: 30_000 });
  await expect(uname, 'org switch must be password-less (no login form)').not.toBeVisible();
  await expect(list).toBeVisible();
}

// Poll the caller's list until the agent reaches a status (default: active), so
// tests that need its SpiceDB owner/org tuples wait for pod SELF-registration —
// the tuples are written when the pod registers, not at preregister.
async function waitForListedStatus(
  page: Page,
  agentId: string,
  re: RegExp = /active/i,
  timeout = 120_000,
) {
  await expect
    .poll(async () => (await listAgents(page)).find((a) => a.agentId === agentId)?.status ?? '', {
      timeout,
    })
    .toMatch(re);
}

// Scenario — the org authorization matrix, proven end-to-end through the REAL
// dashboard → orchestrator → registry → SpiceDB path with SINGLE-ORG tokens (the
// Phase-5 UX). Seed contract (deploy/keycloak/users.tf + scripts/reseed-orgs.sh):
//   root   = PLATFORM master admin (realm `admin` role, member of NO org)
//   acme   = alice (org admin) + viewer (member)
//   globex = bob (org admin)
//   carol  = member of BOTH acme and globex
//   nomad  = member of NO org
// alice is acme's ORG admin ONLY — she is NOT the realm admin (that is root), so
// she has no Global view. The matrix (spec §Authorization matrix, tightened by
// tenant-visibility Phase 2 — least-privilege member visibility):
//   • a member sees ONLY the agents they OWN — a co-member's org agent is
//     invisible: absent from the member's list AND its direct events/logs GET
//     404s (no existence oracle);
//   • an org admin sees + manages every agent in the org;
//   • a user in another org sees none of it (empty list, 404s);
//   • switching orgs is a SILENT re-auth (prompt=none on the SSO session — no
//     password): proven for a multi-org MEMBER (carol: acme ↔ globex);
//   • an org-less non-admin gets the "no organization" screen and cannot spawn;
//   • the platform master admin (root) lands org-less on the Global view and
//     keeps the org-less (Global) spawn path.

const acmeMember = (a: AgentSummary, id: string) => a.agentId === id;

test.describe('organization isolation matrix', () => {
  let aliceCtx: BrowserContext;
  let viewerCtx: BrowserContext;
  let bobCtx: BrowserContext;
  let alice: Page;
  let viewer: Page;
  let bob: Page;
  let acmeAgentId = '';

  test.beforeAll(async ({ browser }) => {
    // Each in its own context (no shared Keycloak SSO cookie), scoped to its org.
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    bobCtx = await contextFor(browser, 'bob', 'globex');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    bob = await bobCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/'), bob.goto('/')]);

    // alice (acme admin) spawns an acme-owned agent the matrix is asserted against.
    // Wait for it to reach `active` — i.e. the pod SELF-registered and its SpiceDB
    // owner/org tuples were written (manage authority depends on them).
    acmeAgentId = await spawn(alice, 'weather-monitor');
    await waitForListedStatus(alice, acmeAgentId, /active/i);
  });

  test.afterAll(async () => {
    if (acmeAgentId && alice) await killAgent(alice, acmeAgentId);
    await aliceCtx?.close();
    await viewerCtx?.close();
    await bobCtx?.close();
  });

  test('a same-org member does NOT see another member’s agent (own-only list + detail 404)', async () => {
    // tenant-visibility Phase 2: a plain member sees ONLY the agents they own.
    // alice's acme agent is owned by alice, so it must NEVER appear in viewer's
    // list — viewer owns none of acme's agents here, so the list excludes it.
    // Poll a few times to prove it stays absent (not merely slow to appear).
    await expect
      .poll(async () => (await listAgents(viewer)).some((a) => a.agentId === acmeAgentId), {
        timeout: 15_000,
      })
      .toBe(false);

    // The card is likewise absent from viewer's rendered dashboard.
    await viewer.reload();
    await expect(viewer.locator('#agent-list')).toBeVisible({ timeout: 30_000 });
    await expect(viewer.locator(`[id="card-${acmeAgentId}"]`)).toHaveCount(0);

    // The REAL boundary: viewer's direct reads AND mutations of a non-owned org
    // agent all 404 (no existence oracle) — a member has neither view nor manage.
    for (const path of [
      { method: 'get', url: `/api/agents/${acmeAgentId}/events` },
      { method: 'get', url: `/api/agents/${acmeAgentId}/logs` },
      { method: 'post', url: `/api/agents/${acmeAgentId}/revoke` },
      { method: 'post', url: `/api/agents/${acmeAgentId}/dismiss` },
      { method: 'delete', url: `/api/agents/${acmeAgentId}` },
    ]) {
      const resp =
        path.method === 'delete'
          ? await viewer.request.delete(path.url)
          : path.method === 'get'
            ? await viewer.request.get(path.url)
            : await viewer.request.post(path.url);
      expect(resp.status(), `${path.method} ${path.url} must 404 for a non-owning member`).toBe(
        404,
      );
    }
  });

  test('the org admin manages every agent in the org', async () => {
    const row = (await listAgents(alice)).find((a) => a.agentId === acmeAgentId);
    expect(row?.canManage, 'the org admin manages every org agent').toBeTruthy();

    // A management action succeeds for the admin (revoke, then resume to leave the
    // agent as we found it). revoke/resume are the cleanest non-destructive proof.
    const revoke = await alice.request.post(`/api/agents/${acmeAgentId}/revoke`);
    expect(revoke.ok(), `admin revoke failed: ${revoke.status()}`).toBeTruthy();
    const resume = await alice.request.post(`/api/agents/${acmeAgentId}/resume`);
    expect(resume.ok(), `admin resume failed: ${resume.status()}`).toBeTruthy();
  });

  test('a user in another org sees none of the first org', async () => {
    // bob (globex) never sees an acme agent.
    expect((await listAgents(bob)).some((a) => a.agentId === acmeAgentId)).toBe(false);
    // And every cross-org direct read/mutate 404s (invisible, no oracle).
    expect((await bob.request.get(`/api/agents/${acmeAgentId}/events`)).status()).toBe(404);
    expect((await bob.request.post(`/api/agents/${acmeAgentId}/revoke`)).status()).toBe(404);
    expect((await bob.request.delete(`/api/agents/${acmeAgentId}`)).status()).toBe(404);
  });

  test('switching between member orgs is a silent re-auth (carol: acme ↔ globex)', async ({
    browser,
  }) => {
    // carol is a plain MEMBER of both acme and globex — the switcher offers her
    // both, and switching between them must be a SILENT (password-less) re-auth on
    // the live Keycloak SSO session. (alice is no longer a realm admin, so her
    // switcher offers NO Global option — only her member orgs; the Global
    // destination is exercised by root below, not here.)
    const carolCtx = await contextFor(browser, 'carol', 'acme');
    try {
      const carol = await carolCtx.newPage();
      await carol.goto('/');
      await expect(carol.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
        timeout: 30_000,
      });

      await switchOrg(carol, 'globex');
      await expect(carol.locator('[data-testid="active-org"]')).toContainText(/globex/i, {
        timeout: 30_000,
      });

      await switchOrg(carol, 'acme');
      await expect(carol.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
        timeout: 30_000,
      });
    } finally {
      await carolCtx.close();
    }
  });

  test('the master admin (root) Global landing is an org directory + counts, NOT an agent list', async ({
    browser,
  }) => {
    // Criterion 5 (tenant-visibility Phase 3): root holds the realm `admin` role and
    // is a member of NO org, so its org-less landing is the platform DIRECTORY — an
    // entry card per tenant + minimal counts — and NOT an agent list. alice (an org
    // admin only) never sees this.
    const rootCtx = await contextFor(browser, 'root');
    try {
      const root = await rootCtx.newPage();
      await root.goto('/');
      await reachDashboardShell(root);
      await expect(root.locator('[data-testid="active-org"]')).toContainText(/global/i, {
        timeout: 30_000,
      });
      // The org directory is shown; the running-agents list is NOT.
      await expect(root.locator('[data-testid="org-directory"]')).toBeVisible({ timeout: 30_000 });
      await expect(root.locator('#agent-list')).toBeHidden();
      // Entry cards for the seeded tenants (acme, globex).
      await expect(root.locator('[data-testid="org-card"][data-org="acme"]')).toBeVisible();
      await expect(root.locator('[data-testid="org-card"][data-org="globex"]')).toBeVisible();
      // Minimal platform counts render (org + agent tallies).
      await expect(root.locator('[data-testid="global-stats"]')).toContainText(/org/i, {
        timeout: 30_000,
      });

      // The master admin keeps the org-less (Global) SPAWN path — asserted via the
      // API (the Global view has no spawn UI): a global agent carries no org and the
      // admin owns + manages it.
      const spawnResp = await root.request.post('/api/spawn', {
        data: { agentType: 'weather-monitor' },
      });
      expect(spawnResp.ok(), `root org-less spawn failed: ${spawnResp.status()}`).toBeTruthy();
      const globalId = (await spawnResp.json()).workloadName as string;
      try {
        await waitForListedStatus(root, globalId, /active|pending/i, 60_000);
        const rec = (await listAgents(root)).find((a) => a.agentId === globalId)!;
        expect(rec?.canManage, 'root manages its own global agent').toBeTruthy();
      } finally {
        await killAgent(root, globalId);
      }
    } finally {
      await rootCtx.close();
    }
  });

  test('the master admin drills into a tenant, manages its agents, and per-tenant scoping holds', async ({
    browser,
  }) => {
    // Criterion 6: root enters acme (view-as-org via the in-place switcher — no
    // re-auth, the org-less token is unchanged), sees acme's agents with canManage
    // true, a revoke/resume round-trip works, then switches to globex and sees ONLY
    // globex — never acme's agent (per-tenant scoping).
    const rootCtx = await contextFor(browser, 'root');
    try {
      const root = await rootCtx.newPage();
      await root.goto('/');
      await reachDashboardShell(root);

      // Drill into acme (in-place, silent — no login form).
      await switchOrg(root, 'acme');
      await expect(root.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
        timeout: 30_000,
      });
      // Sees alice's acme agent, with manage authority (the realm role authorizes).
      const acmeRow = (await listAgents(root)).find((a) => a.agentId === acmeAgentId);
      expect(acmeRow, 'master admin viewing acme must see the acme agent').toBeTruthy();
      expect(acmeRow?.canManage, 'master admin manages the entered tenant’s agents').toBeTruthy();
      // A revoke/resume round-trip succeeds (leaves the agent active for afterAll).
      const revoke = await root.request.post(`/api/agents/${acmeAgentId}/revoke`);
      expect(revoke.ok(), `master-admin revoke failed: ${revoke.status()}`).toBeTruthy();
      const resume = await root.request.post(`/api/agents/${acmeAgentId}/resume`);
      expect(resume.ok(), `master-admin resume failed: ${resume.status()}`).toBeTruthy();

      // Switch to globex: acme's agent must NOT appear (per-tenant scoping).
      await switchOrg(root, 'globex');
      await expect(root.locator('[data-testid="active-org"]')).toContainText(/globex/i, {
        timeout: 30_000,
      });
      expect(
        (await listAgents(root)).some((a) => a.agentId === acmeAgentId),
        'globex view must NOT leak acme’s agent',
      ).toBe(false);
    } finally {
      await rootCtx.close();
    }
  });

  test('the master admin SPAWNS inside the entered tenant and sees it (view-as-org spawn tagging)', async ({
    browser,
  }) => {
    // Regression: root drills into acme (view-as-org) and spawns an agent. Because
    // root carries NO org claim, a naive spawn tagged the agent org-less/global —
    // where the view-as-org list (a.OrgID == viewingOrg) could never match it, so
    // it VANISHED from the view it was created in. The dashboard now forwards the
    // session's viewingOrg as ?org= on spawn and the orchestrator honors it for a
    // realm-admin token, so the agent is created INSIDE acme and appears here. The
    // isolation invariant still holds: switching to globex must NOT show it.
    const rootCtx = await contextFor(browser, 'root');
    let spawnedId = '';
    let root: Page | undefined;
    try {
      root = await rootCtx.newPage();
      await root.goto('/');
      await reachDashboardShell(root);
      await switchOrg(root, 'acme');
      await expect(root.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
        timeout: 30_000,
      });

      spawnedId = await spawn(root, 'weather-monitor');
      // It must appear in the view-as-org list, tagged to acme, manageable by root.
      let row: AgentSummary | undefined;
      await expect
        .poll(async () => {
          row = (await listAgents(root!)).find((a) => a.agentId === spawnedId);
          return row?.orgId ?? '';
        }, { timeout: 60_000 })
        .toBe('acme');
      expect(row?.canManage, 'master admin manages the agent it spawned in the tenant').toBeTruthy();

      // Per-tenant scoping: the acme-tagged agent must not leak into the globex view.
      await switchOrg(root, 'globex');
      await expect(root.locator('[data-testid="active-org"]')).toContainText(/globex/i, {
        timeout: 30_000,
      });
      expect(
        (await listAgents(root)).some((a) => a.agentId === spawnedId),
        'globex view must NOT show the agent spawned in acme',
      ).toBe(false);
    } finally {
      // Realm-admin bypass lets root delete regardless of the current viewing org.
      if (spawnedId && root) await killAgent(root, spawnedId);
      await rootCtx.close();
    }
  });

  test('an org admin (alice) has NO Global option and /api/me reports her non-admin', async () => {
    // Criteria 4 + 8: alice is acme's ORG admin only (no realm role). Her switcher
    // offers her member orgs but NEVER the Global option, and /api/me reports
    // isAdmin=false — the Global view is master-admin-only.
    const me = await alice.request.get('/api/me').then((r) => r.json());
    expect(me.isAdmin, 'alice must not be a realm admin').toBe(false);
    // The org switcher (if shown) must not carry the Global option.
    await alice.goto('/');
    await expect(alice.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
      timeout: 30_000,
    });
    const switcher = alice.locator('[data-testid="org-switcher"]');
    if (await switcher.isVisible()) {
      await expect(switcher.locator('option', { hasText: /global/i })).toHaveCount(0);
    }
  });
});

test.describe('org-less non-admin', () => {
  test('a member of no org gets the "no organization" screen and cannot spawn', async ({
    browser,
  }) => {
    // Explicitly EMPTY storage so this context is guaranteed logged-out,
    // independent of any project-level storageState.
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    try {
      const page = await ctx.newPage();
      // Drive the (identity-first) login WITHOUT an org and DON'T wait for
      // #agent-list — nomad lands on the no-organization gate instead.
      await page.goto('/');
      await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
      await page.locator(LOGIN_USERNAME).fill('nomad');
      const pw = page.locator(LOGIN_PASSWORD);
      if (!(await pw.isVisible())) {
        await page.locator(LOGIN_SUBMIT).click();
        await expect(pw).toBeVisible({ timeout: 30_000 });
      }
      await pw.fill('nomad');
      await page.locator(LOGIN_SUBMIT).click();

      // The no-organization screen shows (not a broken dashboard).
      await expect(page.locator('[data-testid="no-org-screen"]')).toBeVisible({ timeout: 30_000 });

      // The list is empty and a spawn is refused server-side (no active org).
      expect(await listAgents(page)).toEqual([]);
      const spawnResp = await page.request.post('/api/spawn', {
        data: { agentType: 'weather-monitor' },
      });
      expect(spawnResp.status(), 'org-less non-admin spawn must be refused').toBe(403);
    } finally {
      await ctx.close();
    }
  });
});
