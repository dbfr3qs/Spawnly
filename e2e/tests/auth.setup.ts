import { test as setup } from '@playwright/test';
import { AUTH_FILE, DEMO_USER, DEMO_PASS, authFile, login } from '../helpers/auth';

// Authenticate once and persist the session. Every other project depends on
// this (see playwright.config.ts), so the spec files run already-logged-in
// instead of each re-driving the OIDC flow. The dashboard keeps sessions in
// memory, so the cached cookie stays valid for the duration of the run.
//
// alice is acme's ORG admin (not the realm admin, which is `root`). She belongs
// to more than one org (acme + initech), so an org-less login would land her on
// the org PICKER; log her in scoped to acme so the shared storage state is a
// concrete org session the bare-{page} suites can act from.
setup('authenticate as the demo user', async ({ page }) => {
  await login(page, DEMO_USER, DEMO_PASS, 'acme');
  await page.context().storageState({ path: AUTH_FILE });
});

// The platform master admin session (realm `admin`, member of no org → org-less
// Global). The global teardown (global.teardown.ts) runs as this identity so it
// keeps its cross-org sweep power: the per-run org DELETE cascade (signup.spec
// residue) and the whole-platform agent sweep both require the realm admin —
// which alice no longer is. Persisted to its own storage-state file, wired as the
// `cleanup` project's storageState in playwright.config.ts.
setup('authenticate the platform master admin (root)', async ({ page }) => {
  await login(page, 'root', 'root');
  await page.context().storageState({ path: authFile('root') });
});
