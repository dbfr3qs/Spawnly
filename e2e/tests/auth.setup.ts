import { test as setup } from '@playwright/test';
import { AUTH_FILE, login } from '../helpers/auth';

// Authenticate once and persist the session. Every other project depends on
// this (see playwright.config.ts), so the spec files run already-logged-in
// instead of each re-driving the OIDC flow. The dashboard keeps sessions in
// memory, so the cached cookie stays valid for the duration of the run.
setup('authenticate as the demo user', async ({ page }) => {
  await login(page);
  await page.context().storageState({ path: AUTH_FILE });
});
