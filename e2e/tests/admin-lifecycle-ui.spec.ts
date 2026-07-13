import { test, expect, type Browser, type Page } from '@playwright/test';
import { LOGIN_USERNAME, LOGIN_PASSWORD, LOGIN_SUBMIT, login, contextFor, reachDashboardShell } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';
import { verificationLink, clearMessages } from '../helpers/mailpit';

// Master-admin lifecycle UI (signup-lifecycle Phase 5). Drives the dashboard SPA
// as `root` (the platform master admin) on the Global landing, exercising the new
// Users directory + org disable/enable controls end-to-end through the browser:
//   * the Users directory lists a throwaway user;
//   * the root row exposes NO disable/delete controls (lockout guard, cosmetic);
//   * Disable (UI dialog, suspend-agents checkbox) → login blocked + agent revoked;
//   * Enable (UI) → login restored, the suspended agent STAYS suspended;
//   * Disable org (UI) → the member's login is blocked and a Disabled badge shows;
//   * Enable org (UI) → login restored, badge cleared;
//   * Delete (UI) → the user's row disappears;
//   * a non-admin (viewer) sees no admin area and gets 403 on /api/admin/users
//     (regression for criterion 11).
//
// Runs in fresh, logged-out contexts (chromium otherwise reuses alice's session).
// The user + org persist in Keycloak until its next restart, so names are unique
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
// reached the dashboard ('ok') or was held on Keycloak ('blocked') — the
// disabled-account / disabled-org signal. The context is closed before returning.
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
    try {
      // Generous timeout: under full-suite load a genuinely-OK login can render the
      // shell well after 25s; too tight a window would misread a slow success.
      await expect(page.locator('[data-testid="active-org"]')).toBeVisible({ timeout: 45_000 });
      return 'ok';
    } catch {
      // No dashboard shell → distinguish a REAL block from a cold-infra timeout. A
      // genuine block leaves the browser on the (proxied) Keycloak login flow, which
      // takes TWO shapes: the login field re-renders, OR — for a DISABLED account in
      // the identity-first flow — an "account suspended/disabled" error page where
      // #username is gone (Phase 4's accountDisabledMessage). So confirm we're still
      // on Keycloak via ANY of: a login field, the suspended/disabled error text, or
      // the /realms//login-actions/ auth path — not just #username. Only a page that
      // is NEITHER the shell NOR any Keycloak page is truly ambiguous.
      const body = await page.locator('body').innerText().catch(() => '');
      const stillOnKeycloak =
        (await page.locator(LOGIN_USERNAME).isVisible().catch(() => false)) ||
        (await page.locator(LOGIN_PASSWORD).isVisible().catch(() => false)) ||
        /suspended|disabled|invalid username or password/i.test(body) ||
        /\/realms\/|\/login-actions\/|\/resources\//.test(page.url());
      expect(stillOnKeycloak, 'login neither reached the dashboard shell nor stayed on Keycloak — ambiguous, not a proven block').toBe(true);
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

test.describe('master-admin lifecycle UI', () => {
  test.describe.configure({ timeout: 600_000 });

  test('Users directory + org lifecycle actions drive disable/enable/delete', async ({ browser }) => {
    const suffix = uniqueSuffix();
    const username = `uiuser-${suffix}`;
    const password = `Pw-${suffix}-9`;
    const email = `${username}@example.test`;
    const orgName = `UI Org ${suffix}`;
    const enc = encodeURIComponent;

    await clearMessages().catch(() => {});

    const userCtx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const rootCtx = await contextFor(browser, 'root');
    let spawnedId = '';
    let orgId = '';
    let lastGoodUserPage: Page | undefined;

    try {
      // ── Build the throwaway user + org + running agent (self-serve path) ─────
      const userPage = await userCtx.newPage();
      await userPage.goto('/');
      await expect(userPage.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
      await userPage.getByRole('link', { name: /register/i }).first().click();
      await expect(userPage.locator('#username')).toBeVisible({ timeout: 30_000 });
      await userPage.locator('#firstName').fill('UI');
      await userPage.locator('#lastName').fill('Lifecycle');
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
      await expect.poll(() => agentStatus(userPage, spawnedId), { timeout: 90_000 }).toBe('active');
      lastGoodUserPage = userPage;

      // ── Root lands on the Global landing (Users directory + org directory) ───
      const root = await rootCtx.newPage();
      await root.goto('/');
      await reachDashboardShell(root);

      // Resolve the throwaway org id (assertions + cleanup) via the admin API.
      const orgsResp = await root.request.get(`/api/admin/orgs?member=${enc(username)}`);
      expect(orgsResp.ok(), `list user orgs: ${orgsResp.status()}`).toBeTruthy();
      const ownedOrgs: Array<{ orgId?: string }> = await orgsResp.json();
      orgId = ownedOrgs.find((o) => o.orgId)?.orgId ?? '';
      expect(orgId, 'throwaway user should own one org').toBeTruthy();

      const userRow = root.locator(`[data-testid="user-row"][data-user="${username}"]`);
      const rootRow = root.locator('[data-testid="user-row"][data-user="root"]');

      // ── (5) The Users directory lists the new user ──────────────────────────
      await expect(userRow).toBeVisible({ timeout: 30_000 });
      await expect(userRow.locator('[data-testid="user-status"]')).toHaveText('enabled');

      // ── (10) The root row exposes NO disable/delete controls ────────────────
      await expect(rootRow).toBeVisible();
      await expect(rootRow.locator('[data-testid="disable-user-btn"]')).toHaveCount(0);
      await expect(rootRow.locator('[data-testid="delete-user-btn"]')).toHaveCount(0);

      // A FRESH owner session to OBSERVE the agent across the disable. Its access
      // token is valid for the token's full lifetime, so reads survive the KC logout
      // the disable triggers — the setup session (minted minutes ago) could hit a
      // refresh-fail mid-poll and read '' forever, flaking the assertion.
      const ownerObserve = await (await browser.newContext({ storageState: { cookies: [], origins: [] } })).newPage();
      await login(ownerObserve, username, password);
      lastGoodUserPage = ownerObserve;
      expect(await agentStatus(ownerObserve, spawnedId)).toBe('active');

      // ── (6) Disable via the UI with the suspend-agents checkbox ─────────────
      await userRow.locator('[data-testid="disable-user-btn"]').click();
      const disableModal = root.locator('#disable-user-modal');
      await expect(disableModal).toBeVisible();
      await root.locator('[data-testid="disable-suspend-agents"]').check();
      await root.locator('[data-testid="disable-user-confirm"]').click();
      // The row repaints to the disabled state (Enable action now offered).
      await expect(userRow.locator('[data-testid="user-status"]')).toHaveText('disabled', { timeout: 30_000 });
      await expect(userRow.locator('[data-testid="enable-user-btn"]')).toBeVisible();
      // The agent is suspended (observed via the fresh owner session), and a fresh
      // login is blocked.
      await expect.poll(() => agentStatus(ownerObserve, spawnedId), { timeout: 60_000 }).toBe('revoked');
      expect(await attemptLogin(browser, username, password)).toBe('blocked');

      // ── (7) Enable via the UI → login restored, agent STAYS suspended ───────
      await userRow.locator('[data-testid="enable-user-btn"]').click();
      await expect(userRow.locator('[data-testid="user-status"]')).toHaveText('enabled', { timeout: 30_000 });
      expect(await attemptLogin(browser, username, password)).toBe('ok');
      // Re-read the agent from a fresh valid session; it must STAY revoked. A single
      // immediate glance could miss a buggy async auto-resume, so settle-then-check:
      // confirm it remains 'revoked' across several reads over a few seconds.
      const userPage2 = await (await browser.newContext({ storageState: { cookies: [], origins: [] } })).newPage();
      await login(userPage2, username, password);
      lastGoodUserPage = userPage2;
      for (let i = 0; i < 4; i++) {
        expect(await agentStatus(userPage2, spawnedId), 'suspended agent stays suspended after enable').toBe('revoked');
        await userPage2.waitForTimeout(1_000);
      }

      // ── (9) Disable the ORG via the UI → member login blocked + Disabled badge
      const orgCard = root.locator(`[data-testid="org-card"][data-org="${orgId}"]`);
      await expect(orgCard).toBeVisible({ timeout: 30_000 });
      await orgCard.locator('[data-testid="disable-org-btn"]').click();
      const confirmModal = root.locator('#confirm-modal');
      await expect(confirmModal).toBeVisible();
      await root.locator('[data-testid="confirm-ok"]').click();
      await expect(orgCard.locator('[data-testid="org-disabled-badge"]')).toBeVisible({ timeout: 30_000 });
      expect(await attemptLogin(browser, username, password)).toBe('blocked');

      // ── (9) Enable the ORG via the UI → login restored, badge cleared ───────
      await orgCard.locator('[data-testid="enable-org-btn"]').click();
      await expect(orgCard.locator('[data-testid="org-disabled-badge"]')).toHaveCount(0, { timeout: 30_000 });
      expect(await attemptLogin(browser, username, password)).toBe('ok');

      // Kill the agent while we still hold a valid owner session (delete does not
      // stop pods; orphan-to-admin would otherwise leave it running).
      const userPage3 = await (await browser.newContext({ storageState: { cookies: [], origins: [] } })).newPage();
      await login(userPage3, username, password);
      lastGoodUserPage = userPage3;
      await userPage3.request.delete(`/api/agents/${spawnedId}`).catch(() => {});
      spawnedId = '';

      // ── (8) Delete the user via the UI → the row disappears ─────────────────
      await userRow.locator('[data-testid="delete-user-btn"]').click();
      await expect(confirmModal).toBeVisible();
      await root.locator('[data-testid="confirm-ok"]').click();
      await expect(userRow).toHaveCount(0, { timeout: 30_000 });
      expect(await attemptLogin(browser, username, password)).toBe('blocked');

      // ── (11) A non-admin session sees no admin area + is 403'd directly ─────
      const viewerCtx = await contextFor(browser, 'viewer', 'acme');
      try {
        const viewer = await viewerCtx.newPage();
        await viewer.goto('/');
        await expect(viewer.locator('#agent-list')).toBeVisible({ timeout: 30_000 });
        // No master-admin surfaces for a plain member.
        await expect(viewer.locator('[data-testid="users-directory"]')).toBeHidden();
        await expect(viewer.locator('[data-testid="global-directory"]')).toBeHidden();
        // The BFF twin is requireAdmin-gated → 403 on a direct fetch.
        expect((await viewer.request.get('/api/admin/users')).status()).toBe(403);
      } finally {
        await viewerCtx.close();
      }
    } finally {
      if (spawnedId && lastGoodUserPage) {
        await lastGoodUserPage.request.delete(`/api/agents/${spawnedId}`).catch(() => {});
      }
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
