import { test, expect } from '@playwright/test';
import { expectLoginForm } from '../helpers/auth';

// The Keycloak-served login page wears the Spawnly `spawnly` login theme
// (keycloak/themes/spawnly/login, CSS-only child of keycloak.v2). These specs
// prove the theme actually applied, in both palettes.
//
// The computed `body` background-color is the load-bearing assertion: it is the
// signal stock Keycloak would FAIL on (keycloak.v2 paints the page from a
// background-image, not the flat Spawnly `--bg`). The wordmark check alone is
// NOT sufficient — the realm `display_name` is already "Spawnly", so
// "#kc-header-wrapper" reads "Spawnly" even on the unstyled stock page. The
// background assertion also guards against a future keycloak.v2 / PatternFly
// class rename silently un-styling the page (the theme's selectors would miss,
// the background would revert, and this would go red).
test.describe('login theme', () => {
  // Empty session so we see the real logged-out login page — the chromium
  // project otherwise reuses alice's cached session from auth.setup.ts.
  //
  // The theme is dark-by-default (the :root block), with light applied only
  // under `@media (prefers-color-scheme: light)`. Playwright emulates
  // `prefers-color-scheme: light` unless told otherwise, so we set `dark`
  // explicitly here to exercise the theme's default palette (a real dark-OS
  // user's experience); the nested describe below flips it to `light`.
  test.use({ storageState: { cookies: [], origins: [] }, colorScheme: 'dark' });

  test('the login page uses the Spawnly dark palette by default', async ({ page }) => {
    await page.goto('/');
    await expectLoginForm(page);

    // --bg #1a1626 (dashboard dark palette), applied to body#keycloak-bg.
    await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(26, 22, 38)');

    // The Spawnly wordmark (Keycloak realm display_name header) is shown.
    await expect(page.locator('#kc-header-wrapper')).toBeVisible();
  });

  test.describe('with prefers-color-scheme: light', () => {
    test.use({ colorScheme: 'light' });

    test('the login page uses the Spawnly light palette', async ({ page }) => {
      await page.goto('/');
      await expectLoginForm(page);

      // --bg #faf5ff (dashboard light palette).
      await expect(page.locator('body')).toHaveCSS('background-color', 'rgb(250, 245, 255)');

      await expect(page.locator('#kc-header-wrapper')).toBeVisible();
    });
  });
});
