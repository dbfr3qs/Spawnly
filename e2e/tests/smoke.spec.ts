import { test, expect } from '@playwright/test';

// Harness smoke test: proves Playwright can reach the dashboard through the
// port-forward and that the page renders. No agents are spawned.
test('dashboard loads and renders the shell', async ({ page }) => {
  await page.goto('/');
  await expect(page.locator('#agent-list')).toBeVisible();
  await expect(page.locator('#open-modal-btn')).toBeVisible();
});
