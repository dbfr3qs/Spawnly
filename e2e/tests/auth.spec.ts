import { test, expect } from '@playwright/test';
import {
  login,
  DEMO_USER,
  DEMO_PASS,
  LOGIN_USERNAME,
  LOGIN_PASSWORD,
  LOGIN_SUBMIT,
  expectLoginForm,
} from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';

// Unauthenticated behaviour: these run with an empty session, overriding the
// suite-wide cached login, so they see the real logged-out state.
test.describe('unauthenticated', () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  test('the dashboard redirects to the login page when there is no session', async ({ page }) => {
    await page.goto('/');
    // Lands on the (proxied) Keycloak login form, not the dashboard shell.
    await expectLoginForm(page);
    await expect(page.locator('#agent-list')).toHaveCount(0);
  });

  test('the API returns 401 without a session', async ({ page }) => {
    const resp = await page.request.get('/api/agents');
    expect(resp.status()).toBe(401);
  });

  test('a wrong password does not establish a session', async ({ page }) => {
    await page.goto('/');
    await expectLoginForm(page);
    await page.locator(LOGIN_USERNAME).fill(DEMO_USER);
    // Identity-first login: the password field appears on a second screen after
    // submitting the username (realm Organizations enabled). Advance to it if
    // it's not already present, then submit a wrong password.
    const password = page.locator(LOGIN_PASSWORD);
    if (!(await password.isVisible())) {
      await page.locator(LOGIN_SUBMIT).click();
      await expect(password).toBeVisible({ timeout: 30_000 });
    }
    await password.fill('wrong-password');
    await page.locator(LOGIN_SUBMIT).click();
    // Rejected: still on Keycloak's (identity-first) password screen with an
    // error, never the dashboard, and no session. The error page shows the
    // username read-only (no #username input) so we assert the rejection
    // directly; #agent-list absence + the API 401 prove no session was set.
    await expect(page.getByText('Invalid password.')).toBeVisible({ timeout: 30_000 });
    await expect(page.locator('#agent-list')).toHaveCount(0);
    const resp = await page.request.get('/api/agents');
    expect(resp.status()).toBe(401);
  });

  test('login then logout ends the session at the app and the IdP', async ({ page }) => {
    // alice is a multi-org (acme+initech) non-admin now, so an org-less login lands
    // on the org picker — scope her to acme to reach a concrete shell.
    await login(page, DEMO_USER, DEMO_PASS, 'acme');
    // Logged in: the API is now reachable.
    expect((await page.request.get('/api/agents')).ok()).toBeTruthy();

    // Log out via the header button.
    await page.locator('#logout-btn').click();

    // Back to a logged-out state: the app session is gone (API 401) and a fresh
    // visit redirects to the login form again — i.e. the IdP session was killed
    // too (no silent SSO straight back into the dashboard).
    await expectLoginForm(page);
    expect((await page.request.get('/api/agents')).status()).toBe(401);
    await page.goto('/');
    await expectLoginForm(page);
  });
});

// Identity threading: with the suite-wide logged-in session, an agent spawned
// from the UI carries the authenticated user's id (which becomes the token sub),
// not a browser-supplied value — the free-text user field was removed.
test.describe('identity threading', () => {
  test('a spawned agent carries the logged-in user as userId', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#agent-list')).toBeVisible();

    const id = await spawn(page, 'weather-monitor');

    // The orchestrator pre-registers at spawn, so the record (with userId)
    // appears promptly regardless of pod readiness.
    await expect
      .poll(async () => (await listAgents(page)).find((a) => a.agentId === id)?.userId, {
        timeout: 30_000,
      })
      .toBe(DEMO_USER);
  });
});
