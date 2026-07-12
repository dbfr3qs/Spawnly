import { test, expect, type Browser, type Page } from '@playwright/test';
import { LOGIN_USERNAME, LOGIN_PASSWORD, LOGIN_SUBMIT, login, contextFor } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';
import { verificationLink, clearMessages } from '../helpers/mailpit';

// Master-admin USER lifecycle (signup-lifecycle Phase 3, spec criteria 5-8, 10, 11).
// API-driven as `root` (the platform master admin) against a THROWAWAY verified
// user who owns a throwaway org with a running agent:
//   * disable WITHOUT suspend → fresh login blocked, the agent keeps running;
//   * enable → login works again;
//   * disable WITH suspendAgents → login blocked AND the agent is suspended (revoked);
//   * enable → login restored, the agent STAYS suspended (asymmetric resume);
//   * delete → the user is gone from GET /api/admin/users and login is impossible;
//   * root disable/delete → 403 (lockout guard); a non-admin GET /api/admin/users → 403.
//
// Runs in fresh, logged-out contexts (the chromium project otherwise reuses alice's
// cached session). The user + org persist in Keycloak until its next restart, so the
// username/email are unique per run to stay idempotent across repeated `make e2e`.

function uniqueSuffix(): string {
  return `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}`
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');
}

// verifyEmail completes Phase-1 email verification: poll Mailpit for the mail sent
// to `email`, visit the action-token link, and log in with the just-set credentials
// if Keycloak drops back to its login form (fresh action-token context).
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
      await page.locator(LOGIN_SUBMIT).click(); // identity-first → password screen
      await expect(pw).toBeVisible({ timeout: 30_000 });
    }
    await pw.fill(pass);
    await page.locator(LOGIN_SUBMIT).click();
  }
}

// finishReauth completes the post-create-org /switch-org re-auth (NOT password-less
// under realm Organizations), landing on the dashboard shell.
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
// reached the dashboard ('ok') or was held on Keycloak ('blocked') — the
// disabled-account signal. The context is closed before returning (we only want
// the outcome, not a lingering session).
async function attemptLogin(browser: Browser, user: string, pass: string): Promise<'ok' | 'blocked'> {
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  try {
    const page = await ctx.newPage();
    await page.goto('/');
    await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
    await page.locator(LOGIN_USERNAME).fill(user);
    const pw = page.locator(LOGIN_PASSWORD);
    if (!(await pw.isVisible().catch(() => false))) {
      await page.locator(LOGIN_SUBMIT).click(); // identity-first → password (or an error page)
      await page.waitForTimeout(1_000);
    }
    if (await pw.isVisible().catch(() => false)) {
      await pw.fill(pass);
      await page.locator(LOGIN_SUBMIT).click();
    }
    // A successful login redirects to the dashboard shell (active-org indicator);
    // a disabled account never leaves Keycloak.
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

// agentStatus reads one agent's status via the given (authenticated) page, or ''.
async function agentStatus(page: Page, agentId: string): Promise<string> {
  const agents = await listAgents(page);
  return agents.find((a) => a.agentId === agentId)?.status ?? '';
}

test.describe('master-admin user lifecycle', () => {
  test.describe.configure({ timeout: 600_000 });

  test('disable/enable/delete a user, with agent suspension + root guard', async ({ browser }) => {
    const suffix = uniqueSuffix();
    const username = `life-${suffix}`;
    const password = `Pw-${suffix}-9`;
    const email = `${username}@example.test`;
    const orgName = `Life Org ${suffix}`;
    const enc = encodeURIComponent;

    await clearMessages().catch(() => {});

    const userCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const rootCtx = await contextFor(browser, 'root');
    let spawnedId = '';
    let orgId = '';
    let lastGoodUserPage: Page | undefined;

    try {
      // ── Build the throwaway user + org + running agent ──────────────────────
      const userPage = await userCtx.newPage();
      await userPage.goto('/');
      await expect(userPage.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
      await userPage.getByRole('link', { name: /register/i }).first().click();
      await expect(userPage.locator('#username')).toBeVisible({ timeout: 30_000 });
      await userPage.locator('#firstName').fill('Life');
      await userPage.locator('#lastName').fill('Cycle');
      await userPage.locator('#email').fill(email);
      await userPage.locator('#username').fill(username);
      await userPage.locator('#password').fill(password);
      await userPage.locator('#password-confirm').fill(password);
      await userPage.locator('#kc-form-buttons input[type="submit"]').click();
      await verifyEmail(userPage, email, username, password);

      await expect(userPage.locator('[data-testid="create-org-form"]')).toBeVisible({ timeout: 60_000 });
      await userPage.locator('[data-testid="create-org-name"]').fill(orgName);
      const [signupResp] = await Promise.all([
        userPage.waitForResponse(
          (r) => r.url().includes('/api/signup/org') && r.request().method() === 'POST',
        ),
        userPage.locator('[data-testid="create-org-submit"]').click(),
      ]);
      expect(signupResp.status(), 'create org').toBe(201);
      await finishReauth(userPage, username, password);

      spawnedId = await spawn(userPage, 'weather-monitor');
      await expect
        .poll(() => agentStatus(userPage, spawnedId), { timeout: 90_000 })
        .toBe('active');
      lastGoodUserPage = userPage;

      const root = await rootCtx.newPage();
      await root.goto('/');

      // Resolve the throwaway org id (for cleanup + assertions) via the admin API.
      const orgsResp = await root.request.get(`/api/admin/orgs?member=${enc(username)}`);
      expect(orgsResp.ok(), `list user orgs: ${orgsResp.status()}`).toBeTruthy();
      const orgs: Array<{ orgId?: string }> = await orgsResp.json();
      orgId = orgs.find((o) => o.orgId)?.orgId ?? '';
      expect(orgId, 'throwaway user should own one org').toBeTruthy();

      // ── (5) The master admin lists all users; the new user is present ───────
      const listResp = await root.request.get('/api/admin/users');
      expect(listResp.ok(), `list users: ${listResp.status()}`).toBeTruthy();
      const users: Array<{ username?: string; enabled?: boolean }> = await listResp.json();
      const seeded = users.find((u) => u.username === username);
      expect(seeded, 'new user must appear in the admin user directory').toBeTruthy();
      expect(seeded?.enabled, 'new user starts enabled').toBe(true);

      // ── (10) root lockout guard + (11) requireAdmin ─────────────────────────
      expect((await root.request.post('/api/admin/users/root/disable', { data: {} })).status()).toBe(403);
      expect((await root.request.delete('/api/admin/users/root')).status()).toBe(403);
      // A non-admin session (viewer) must be forbidden from the admin surface.
      const viewerCtx = await contextFor(browser, 'viewer', 'acme');
      try {
        const viewer = await viewerCtx.newPage();
        await viewer.goto('/');
        expect((await viewer.request.get('/api/admin/users')).status()).toBe(403);
      } finally {
        await viewerCtx.close();
      }

      // ── (6) disable WITHOUT suspend → login blocked, agent keeps running ────
      expect(
        (await root.request.post(`/api/admin/users/${enc(username)}/disable`, {
          data: { suspendAgents: false },
        })).status(),
      ).toBe(204);
      // The agent is untouched (read via the still-valid pre-disable user session).
      expect(await agentStatus(userPage, spawnedId)).toBe('active');
      expect(await attemptLogin(browser, username, password)).toBe('blocked');

      // ── (7) enable → login works again ──────────────────────────────────────
      expect((await root.request.post(`/api/admin/users/${enc(username)}/enable`)).status()).toBe(204);
      expect(await attemptLogin(browser, username, password)).toBe('ok');

      // A fresh, valid session to observe agent status across the next disable.
      const userPage2 = await (await browser.newContext({ storageState: { cookies: [], origins: [] } })).newPage();
      await login(userPage2, username, password);
      lastGoodUserPage = userPage2;
      expect(await agentStatus(userPage2, spawnedId)).toBe('active');

      // ── (6) disable WITH suspendAgents → login blocked AND agent suspended ──
      expect(
        (await root.request.post(`/api/admin/users/${enc(username)}/disable`, {
          data: { suspendAgents: true },
        })).status(),
      ).toBe(204);
      await expect
        .poll(() => agentStatus(userPage2, spawnedId), { timeout: 60_000 })
        .toBe('revoked');
      expect(await attemptLogin(browser, username, password)).toBe('blocked');

      // ── (7) enable → login restored, agent STAYS suspended ──────────────────
      expect((await root.request.post(`/api/admin/users/${enc(username)}/enable`)).status()).toBe(204);
      expect(await attemptLogin(browser, username, password)).toBe('ok');
      const userPage3 = await (await browser.newContext({ storageState: { cookies: [], origins: [] } })).newPage();
      await login(userPage3, username, password);
      lastGoodUserPage = userPage3;
      expect(await agentStatus(userPage3, spawnedId), 'suspended agent stays suspended after enable').toBe('revoked');

      // Kill the agent while we still hold a valid owner session (delete does not
      // stop pods; orphan-to-admin keeps it running otherwise).
      await userPage3.request.delete(`/api/agents/${spawnedId}`).catch(() => {});
      spawnedId = '';

      // ── (8) delete → user gone from the directory, login impossible ─────────
      expect((await root.request.delete(`/api/admin/users/${enc(username)}`)).status()).toBe(204);
      const afterResp = await root.request.get('/api/admin/users');
      const after: Array<{ username?: string }> = await afterResp.json();
      expect(after.some((u) => u.username === username), 'deleted user must be gone').toBe(false);
      expect(await attemptLogin(browser, username, password)).toBe('blocked');
    } finally {
      if (spawnedId && lastGoodUserPage) {
        await lastGoodUserPage.request.delete(`/api/agents/${spawnedId}`).catch(() => {});
      }
      // Tidy the throwaway org through the admin API (best-effort).
      if (orgId) {
        const root = await rootCtx.newPage().catch(() => undefined);
        if (root) {
          await root.goto('/').catch(() => {});
          await root.request.delete(`/api/admin/orgs/${enc(orgId)}`).catch(() => {});
        }
      }
      await userCtx.close().catch(() => {});
      await rootCtx.close().catch(() => {});
    }
  });
});
