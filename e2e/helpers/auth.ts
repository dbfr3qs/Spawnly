import { expect, type Page } from '@playwright/test';

// Where the authenticated session is cached so the rest of the suite reuses it
// (see auth.setup.ts and the `setup` project dependency in playwright.config.ts).
export const AUTH_FILE = 'playwright/.auth/user.json';

// The seeded demo user in IdentityServer (identityserver/TestUsers.cs).
export const DEMO_USER = 'alice';
export const DEMO_PASS = 'alice';

// Drive the real OIDC login UI: navigating to the dashboard while logged out
// redirects to the (proxied) IdentityServer login page; fill it and submit. The
// redirect chain lands back on the dashboard shell. Asserts success by waiting
// for #agent-list, which only renders on the dashboard.
export async function login(
  page: Page,
  user: string = DEMO_USER,
  pass: string = DEMO_PASS,
): Promise<void> {
  await page.goto('/');
  // asp-for="Username"/"Password" render inputs with these ids.
  await expect(page.locator('#Username')).toBeVisible({ timeout: 30_000 });
  await page.locator('#Username').fill(user);
  await page.locator('#Password').fill(pass);
  await page.locator('button[type="submit"]').click();
  await expect(page.locator('#agent-list')).toBeVisible({ timeout: 30_000 });
}
