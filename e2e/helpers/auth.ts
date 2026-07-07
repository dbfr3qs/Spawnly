import { expect, type Page } from '@playwright/test';

// Where the authenticated session is cached so the rest of the suite reuses it
// (see auth.setup.ts and the `setup` project dependency in playwright.config.ts).
export const AUTH_FILE = 'playwright/.auth/user.json';

// The seeded demo user in the Keycloak realm (deploy/keycloak/, provisioned by
// scripts/bootstrap.sh as alice/alice — the admin).
export const DEMO_USER = 'alice';
export const DEMO_PASS = 'alice';

// Keycloak's login form field selectors. The default login theme renders the
// username/password inputs as #username/#password and the submit button as
// #kc-login. Centralised here so the spec files assert "the login form is
// showing" without hardcoding Keycloak-specific ids (the previous
// IdentityServer form used #Username/#Password/button[type=submit]).
export const LOGIN_USERNAME = '#username';
export const LOGIN_PASSWORD = '#password';
export const LOGIN_SUBMIT = '#kc-login';

// expectLoginForm asserts the browser is on the (proxied) Keycloak login page —
// i.e. the username field is visible. Used by the unauthenticated specs to
// confirm a logged-out / rejected state.
export async function expectLoginForm(page: Page): Promise<void> {
  await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
}

// Drive the real OIDC login UI: navigating to the dashboard while logged out
// redirects to the (proxied) Keycloak login page; fill it and submit. The
// redirect chain lands back on the dashboard shell. Asserts success by waiting
// for #agent-list, which only renders on the dashboard.
//
// Handles Keycloak's IDENTITY-FIRST (username-first) login: with realm
// Organizations enabled, the browser flow presents username on the first screen
// and password on a second (so it can detect an org's brokered IdP before
// asking for a password). The password field is therefore not guaranteed to be
// present alongside the username field — we submit the username and wait for the
// password screen. If the deployment uses the classic single-screen form
// (password already present), the same code path still works.
export async function login(
  page: Page,
  user: string = DEMO_USER,
  pass: string = DEMO_PASS,
): Promise<void> {
  await page.goto('/');
  await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
  await page.locator(LOGIN_USERNAME).fill(user);

  const password = page.locator(LOGIN_PASSWORD);
  if (!(await password.isVisible())) {
    // Identity-first: submit the username to advance to the password screen.
    await page.locator(LOGIN_SUBMIT).click();
    await expect(password).toBeVisible({ timeout: 30_000 });
  }
  await password.fill(pass);
  await page.locator(LOGIN_SUBMIT).click();
  await expect(page.locator('#agent-list')).toBeVisible({ timeout: 30_000 });
}
