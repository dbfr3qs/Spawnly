import { test, expect } from '@playwright/test';
import { login, DEMO_USER } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';

// Unauthenticated behaviour: these run with an empty session, overriding the
// suite-wide cached login, so they see the real logged-out state.
test.describe('unauthenticated', () => {
  test.use({ storageState: { cookies: [], origins: [] } });

  test('the dashboard redirects to the login page when there is no session', async ({ page }) => {
    await page.goto('/');
    // Lands on the (proxied) IdentityServer login form, not the dashboard shell.
    await expect(page.locator('#Username')).toBeVisible({ timeout: 30_000 });
    await expect(page.locator('#agent-list')).toHaveCount(0);
  });

  test('the API returns 401 without a session', async ({ page }) => {
    const resp = await page.request.get('/api/agents');
    expect(resp.status()).toBe(401);
  });

  test('a wrong password does not establish a session', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#Username')).toBeVisible({ timeout: 30_000 });
    await page.locator('#Username').fill(DEMO_USER);
    await page.locator('#Password').fill('wrong-password');
    await page.locator('button[type="submit"]').click();
    // Still on the login page with an error; no dashboard, no session.
    await expect(page.locator('#agent-list')).toHaveCount(0);
    await expect(page.locator('#Username')).toBeVisible();
    const resp = await page.request.get('/api/agents');
    expect(resp.status()).toBe(401);
  });

  test('login then logout ends the session at the app and the IdP', async ({ page }) => {
    await login(page);
    // Logged in: the API is now reachable.
    expect((await page.request.get('/api/agents')).ok()).toBeTruthy();

    // Log out via the header button.
    await page.locator('#logout-btn').click();

    // Back to a logged-out state: the app session is gone (API 401) and a fresh
    // visit redirects to the login form again — i.e. the IdP session was killed
    // too (no silent SSO straight back into the dashboard).
    await expect(page.locator('#Username')).toBeVisible({ timeout: 30_000 });
    expect((await page.request.get('/api/agents')).status()).toBe(401);
    await page.goto('/');
    await expect(page.locator('#Username')).toBeVisible({ timeout: 30_000 });
  });
});

// Identity threading: with the suite-wide logged-in session, an agent spawned
// from the UI carries the authenticated user's id (which becomes the token sub),
// not a browser-supplied value — the free-text user field was removed.
test.describe('identity threading', () => {
  test('a spawned agent carries the logged-in user as userId', async ({ page }) => {
    await page.goto('/');
    await expect(page.locator('#agent-list')).toBeVisible();

    const id = await spawn(page, 'worker');

    // The orchestrator pre-registers at spawn, so the record (with userId)
    // appears promptly regardless of pod readiness.
    await expect
      .poll(async () => (await listAgents(page)).find((a) => a.agentId === id)?.userId, {
        timeout: 30_000,
      })
      .toBe(DEMO_USER);
  });
});
