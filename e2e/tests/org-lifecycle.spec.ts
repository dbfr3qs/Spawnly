import { test, expect, type Browser, type Page } from '@playwright/test';
import { LOGIN_USERNAME, LOGIN_PASSWORD, LOGIN_SUBMIT, login, contextFor } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';
import { verificationLink, clearMessages } from '../helpers/mailpit';

// Org lifecycle (signup-lifecycle Phase 4, spec criterion 9 + the spawn guard).
// API-driven as `root` (the platform master admin) against a THROWAWAY org:
//   * member A (self-serve creator/admin) owns a running agent in the org;
//   * member B is added, then INDEPENDENTLY disabled at the user level (Phase 3);
//   * disable the org → A's login is blocked, the agent is suspended (revoked),
//     and a spawn into the org is refused;
//   * enable the org → A logs in again, B is STILL disabled (no-clobber), the
//     agent stays suspended (asymmetric resume);
//   * manual resume → the agent is active again;
//   * delete the org → gone, A's account stays enabled.
//
// Runs in fresh, logged-out contexts (chromium otherwise reuses alice's session).
// The users + org persist in Keycloak until its next restart, so names are unique
// per run to stay idempotent across repeated `make e2e`.

function uniqueSuffix(): string {
  return `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}`
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');
}

// verifyEmail completes Phase-1 email verification: poll Mailpit for the mail sent
// to `email`, visit the action-token link, and log in if Keycloak drops back to
// its login form (fresh action-token context).
async function verifyEmail(page: Page, email: string, user: string, pass: string) {
  const link = await verificationLink(email);
  await page.goto(link);
  const noOrg = page.locator('[data-testid="no-org-screen"]');
  const uname = page.locator(LOGIN_USERNAME);
  await expect(noOrg.or(uname).first()).toBeVisible({ timeout: 60_000 });
  if (await uname.isVisible().catch(() => false)) {
    await uname.fill(user);
    const pw = page.locator(LOGIN_PASSWORD);
    if (!(await pw.isVisible())) {
      await page.locator(LOGIN_SUBMIT).click();
      await expect(pw).toBeVisible({ timeout: 30_000 });
    }
    await pw.fill(pass);
    await page.locator(LOGIN_SUBMIT).click();
  }
}

// finishReauth completes the post-create-org /switch-org re-auth (NOT
// password-less under realm Organizations), landing on the dashboard shell.
async function finishReauth(page: Page, user: string, pass: string) {
  await expect(async () => {
    const onDash = await page.locator('#agent-list').isVisible().catch(() => false);
    const onForm = await page.locator(LOGIN_USERNAME).isVisible().catch(() => false);
    expect(onDash || onForm).toBeTruthy();
  }).toPass({ timeout: 30_000 });
  if (await page.locator(LOGIN_USERNAME).isVisible().catch(() => false)) {
    await page.locator(LOGIN_USERNAME).fill(user);
    const pw = page.locator(LOGIN_PASSWORD);
    if (!(await pw.isVisible())) {
      await page.locator(LOGIN_SUBMIT).click();
      await expect(pw).toBeVisible({ timeout: 30_000 });
    }
    await pw.fill(pass);
    await page.locator(LOGIN_SUBMIT).click();
  }
  await expect(page.locator('#agent-list')).toBeVisible({ timeout: 60_000 });
}

// attemptLogin logs in from a FRESH, logged-out context and reports whether it
// reached the dashboard ('ok') or was held on Keycloak ('blocked').
async function attemptLogin(browser: Browser, user: string, pass: string): Promise<'ok' | 'blocked'> {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  try {
    const page = await ctx.newPage();
    await page.goto('/');
    await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
    await page.locator(LOGIN_USERNAME).fill(user);
    const pw = page.locator(LOGIN_PASSWORD);
    if (!(await pw.isVisible().catch(() => false))) {
      await page.locator(LOGIN_SUBMIT).click();
      await page.waitForTimeout(1_000);
    }
    if (await pw.isVisible().catch(() => false)) {
      await pw.fill(pass);
      await page.locator(LOGIN_SUBMIT).click();
    }
    try {
      await expect(page.locator('[data-testid="active-org"]')).toBeVisible({ timeout: 25_000 });
      return 'ok';
    } catch {
      return 'blocked';
    }
  } finally {
    await ctx.close();
  }
}

async function agentStatus(page: Page, agentId: string): Promise<string> {
  const agents = await listAgents(page);
  return agents.find((a) => a.agentId === agentId)?.status ?? '';
}

async function userEnabled(page: Page, username: string): Promise<boolean | undefined> {
  const resp = await page.request.get('/api/admin/users');
  if (!resp.ok()) return undefined;
  const users: Array<{ username?: string; enabled?: boolean }> = await resp.json();
  return users.find((u) => u.username === username)?.enabled;
}

test.describe('org lifecycle', () => {
  test.describe.configure({ timeout: 600_000 });

  test('disable/enable/delete an org: member lockout, agent suspension, spawn guard, no-clobber', async ({
    browser,
  }) => {
    const suffix = uniqueSuffix();
    const userA = `orga-${suffix}`;
    const passA = `Pw-${suffix}-9`;
    const emailA = `${userA}@example.test`;
    const userB = `orgb-${suffix}`;
    const orgName = `Org Life ${suffix}`;
    const enc = encodeURIComponent;

    await clearMessages().catch(() => {});

    const userCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const rootCtx = await contextFor(browser, 'root');
    let spawnedId = '';
    let orgId = '';

    try {
      // ── Build the throwaway org (member A = admin) + a running agent ─────────
      const userPageA = await userCtx.newPage();
      await userPageA.goto('/');
      await expect(userPageA.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
      await userPageA.getByRole('link', { name: /register/i }).first().click();
      await expect(userPageA.locator('#username')).toBeVisible({ timeout: 30_000 });
      await userPageA.locator('#firstName').fill('Org');
      await userPageA.locator('#lastName').fill('Life');
      await userPageA.locator('#email').fill(emailA);
      await userPageA.locator('#username').fill(userA);
      await userPageA.locator('#password').fill(passA);
      await userPageA.locator('#password-confirm').fill(passA);
      await userPageA.locator('#kc-form-buttons input[type="submit"]').click();
      await verifyEmail(userPageA, emailA, userA, passA);

      await expect(userPageA.locator('[data-testid="create-org-form"]')).toBeVisible({ timeout: 60_000 });
      await userPageA.locator('[data-testid="create-org-name"]').fill(orgName);
      const [signupResp] = await Promise.all([
        userPageA.waitForResponse(
          (r) => r.url().includes('/api/signup/org') && r.request().method() === 'POST',
        ),
        userPageA.locator('[data-testid="create-org-submit"]').click(),
      ]);
      expect(signupResp.status(), 'create org').toBe(201);
      await finishReauth(userPageA, userA, passA);

      spawnedId = await spawn(userPageA, 'weather-monitor');
      await expect.poll(() => agentStatus(userPageA, spawnedId), { timeout: 90_000 }).toBe('active');

      const root = await rootCtx.newPage();
      await root.goto('/');

      // Resolve the org id (cleanup + assertions).
      const orgsResp = await root.request.get(`/api/admin/orgs?member=${enc(userA)}`);
      expect(orgsResp.ok(), `list user orgs: ${orgsResp.status()}`).toBeTruthy();
      const orgs: Array<{ orgId?: string }> = await orgsResp.json();
      orgId = orgs.find((o) => o.orgId)?.orgId ?? '';
      expect(orgId, 'member A should own one org').toBeTruthy();

      // ── Add member B, then INDEPENDENTLY disable them (user-level) ───────────
      expect(
        (await root.request.post(`/api/admin/orgs/${enc(orgId)}/members`, {
          data: { user: userB, role: 'member' },
        })).status(),
        'add member B',
      ).toBe(201);
      expect(
        (await root.request.post(`/api/admin/users/${enc(userB)}/disable`, { data: {} })).status(),
        'independently disable B',
      ).toBe(204);
      expect(await userEnabled(root, userB), 'B disabled independently').toBe(false);

      // ── (11) requireAdmin: a non-admin cannot drive org lifecycle ───────────
      const viewerCtx = await contextFor(browser, 'viewer', 'acme');
      try {
        const viewer = await viewerCtx.newPage();
        await viewer.goto('/');
        expect((await viewer.request.post(`/api/admin/orgs/${enc(orgId)}/disable`)).status()).toBe(403);
        expect((await viewer.request.post(`/api/admin/orgs/${enc(orgId)}/enable`)).status()).toBe(403);
      } finally {
        await viewerCtx.close();
      }

      // ── (9) DISABLE the org ─────────────────────────────────────────────────
      expect((await root.request.post(`/api/admin/orgs/${enc(orgId)}/disable`)).status(), 'disable org').toBe(
        200,
      );

      // Member A's login is now blocked natively (KC account disabled).
      expect(await attemptLogin(browser, userA, passA), 'A blocked after org disable').toBe('blocked');

      // The org agent is suspended — read via A's still-valid pre-disable session.
      await expect
        .poll(() => agentStatus(userPageA, spawnedId), { timeout: 60_000 })
        .toBe('revoked');

      // A spawn into the disabled org is refused (defense-in-depth guard). Use A's
      // still-fresh session so the request reaches the orchestrator → registry
      // guard; the response must be non-ok (never a new agent in a disabled org).
      const guardResp = await userPageA.request.post('/api/spawn', {
        data: { agentType: 'weather-monitor' },
      });
      expect(guardResp.ok(), `spawn into disabled org must be refused (got ${guardResp.status()})`).toBeFalsy();

      // (A/3) A member-mutation via the ORG-ADMIN self-service path is refused while
      // the org is disabled. userPageA is the org admin; its dashboard session
      // OUTLIVES the KC session revoke, so this is the exact stale-org-admin-token
      // scenario the guard defends: removing a member re-enables them, laundering
      // them out of the lockout. The registry never sees the DELETE (guard → 403).
      const staleRemove = await userPageA.request.delete(`/api/org/members/${enc(userB)}`);
      expect(
        staleRemove.status(),
        `stale org-admin member-remove on disabled org must be 403 (got ${staleRemove.status()})`,
      ).toBe(403);
      // B is untouched by the refused call (still a disabled member, not re-enabled).
      expect(await userEnabled(root, userB), 'B stays disabled — refused remove did not re-enable').toBe(false);

      // ── (9) ENABLE the org ──────────────────────────────────────────────────
      expect((await root.request.post(`/api/admin/orgs/${enc(orgId)}/enable`)).status(), 'enable org').toBe(
        200,
      );
      expect(await attemptLogin(browser, userA, passA), 'A can log in again').toBe('ok');
      // B stays disabled — the org only re-enables members IT disabled (no clobber).
      expect(await userEnabled(root, userB), 'B still disabled after org enable').toBe(false);

      // Agent still suspended — observe via a FRESH member-A session.
      const aCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
      try {
        const aPage = await aCtx.newPage();
        await login(aPage, userA, passA);
        expect(await agentStatus(aPage, spawnedId), 'agent stays suspended after enable').toBe('revoked');

        // ── Manual resume restores the agent ──────────────────────────────────
        const resumeResp = await aPage.request.post(`/api/agents/${enc(spawnedId)}/resume`);
        expect(resumeResp.ok(), `manual resume: ${resumeResp.status()}`).toBeTruthy();
        await expect.poll(() => agentStatus(aPage, spawnedId), { timeout: 60_000 }).toBe('active');

        // Kill the agent while we still hold a valid owner session.
        await aPage.request.delete(`/api/agents/${enc(spawnedId)}`).catch(() => {});
        spawnedId = '';
      } finally {
        await aCtx.close();
      }

      // ── (9) DELETE the org → gone; A's account stays enabled ────────────────
      expect((await root.request.delete(`/api/admin/orgs/${enc(orgId)}`)).status(), 'delete org').toBe(204);
      const afterOrgs = await root.request.get(`/api/admin/orgs?member=${enc(userA)}`);
      const remaining: Array<{ orgId?: string }> = await afterOrgs.json();
      expect(remaining.some((o) => o.orgId === orgId), 'org must be gone').toBe(false);
      expect(await userEnabled(root, userA), 'A stays enabled after org delete').toBe(true);
      orgId = '';
    } finally {
      if (orgId) {
        const root = await rootCtx.newPage().catch(() => undefined);
        if (root) {
          await root.goto('/').catch(() => {});
          await root.request.post(`/api/admin/orgs/${enc(orgId)}/enable`).catch(() => {});
          await root.request.delete(`/api/admin/orgs/${enc(orgId)}`).catch(() => {});
        }
      }
      // Best-effort: remove member B's throwaway account.
      const cleanup = await rootCtx.newPage().catch(() => undefined);
      if (cleanup) {
        await cleanup.goto('/').catch(() => {});
        await cleanup.request.delete(`/api/admin/users/${enc(userB)}`).catch(() => {});
      }
      await userCtx.close().catch(() => {});
      await rootCtx.close().catch(() => {});
    }
  });
});
