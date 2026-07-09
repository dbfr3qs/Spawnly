import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import {
  contextFor,
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
//   acme   = alice (org admin) + viewer (member)
//   globex = bob (org admin)
//   nomad  = member of NO org
// The matrix (spec §Authorization matrix):
//   • a member sees every org agent but manages only their own (read-only on
//     others) — and CANNOT mutate another's agent (UI absent AND the direct API
//     404s);
//   • an org admin manages every agent in the org;
//   • a user in another org sees none of it (empty list, 404s);
//   • switching orgs is a SILENT re-auth (prompt=none on the SSO session — no password);
//   • an org-less non-admin gets the "no organization" screen and cannot spawn;
//   • a realm admin keeps the org-less (Global) spawn path.

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

  test('a same-org member sees the agent READ-ONLY and cannot mutate it', async () => {
    // Visible in the member's list, flagged canManage=false.
    await expect
      .poll(async () => (await listAgents(viewer)).some((a) => a.agentId === acmeAgentId), {
        timeout: 30_000,
      })
      .toBe(true);
    const row = (await listAgents(viewer)).find((a) => a.agentId === acmeAgentId)!;
    expect(row.canManage, 'a member must see another owner’s agent as read-only').toBeFalsy();

    // Render the card and assert the mutating controls are ABSENT (cosmetic gate).
    await viewer.reload();
    const card = viewer.locator(`[id="card-${acmeAgentId}"]`);
    await expect(card).toBeVisible({ timeout: 30_000 });
    await expect(card.locator('[data-testid="revoke"]')).toHaveCount(0);
    await expect(card.locator('[data-testid="kill"]')).toHaveCount(0);

    // The REAL boundary: direct API calls must 404 (no existence oracle), so a
    // tampered client that forges the buttons still can't mutate the agent.
    for (const path of [
      { method: 'post', url: `/api/agents/${acmeAgentId}/revoke` },
      { method: 'post', url: `/api/agents/${acmeAgentId}/dismiss` },
      { method: 'delete', url: `/api/agents/${acmeAgentId}` },
    ]) {
      const resp =
        path.method === 'delete'
          ? await viewer.request.delete(path.url)
          : await viewer.request.post(path.url);
      expect(resp.status(), `${path.method} ${path.url} must 404 for a member`).toBe(404);
    }
    // ...and the agent is untouched.
    expect((await listAgents(viewer)).some((a) => a.agentId === acmeAgentId)).toBe(true);
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

  test('switching orgs is a silent re-auth (acme ↔ Global for the realm admin)', async () => {
    // alice starts in acme.
    await alice.goto('/');
    await expect(alice.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
      timeout: 30_000,
    });

    // Switch to Global (org-less) — a silent (password-less) re-auth through /switch-org.
    await switchOrg(alice);
    await expect(alice.locator('[data-testid="active-org"]')).toContainText(/global/i, {
      timeout: 30_000,
    });

    // Switch back into acme so the rest of the suite/cleanup runs as the acme admin.
    await switchOrg(alice, 'acme');
    await expect(alice.locator('[data-testid="active-org"]')).toContainText(/acme/i, {
      timeout: 30_000,
    });
  });

  test('a realm admin keeps the org-less (Global) spawn path', async () => {
    await switchOrg(alice); // Global
    await expect(alice.locator('[data-testid="active-org"]')).toContainText(/global/i, {
      timeout: 30_000,
    });
    const globalId = await spawn(alice, 'weather-monitor');
    try {
      await waitForListedStatus(alice, globalId, /active|pending/i, 60_000);
      // A global (org-less) agent carries no orgId; the admin owns and manages it.
      const rec = (await listAgents(alice)).find((a) => a.agentId === globalId)!;
      expect(rec.canManage).toBeTruthy();
    } finally {
      await killAgent(alice, globalId);
      await switchOrg(alice, 'acme');
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
