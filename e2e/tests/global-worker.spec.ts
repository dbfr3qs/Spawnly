import { test, expect } from '@playwright/test';
import { spawn, waitForStatus } from '../helpers/dashboard';
import { getEvents } from '../helpers/events';
import { killTrees } from '../helpers/cleanup';

// Scenario 1 — smoke test. Spawn a global-worker (short-lived, no LLM: it calls
// a tenant-agnostic sample API and exits) and confirm it runs to completion.
// This is the fast, deterministic anchor for the suite.
test.describe('global-worker', () => {
  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    await killTrees(page, spawned.splice(0));
  });

  test('spawns and runs to completion', async ({ page }) => {
    await page.goto('/');

    const id = await spawn(page, 'global-worker');
    spawned.push(id);

    // Pod scheduling + image start dominate; waitForStatus carries the long
    // default timeout.
    await waitForStatus(page, id, 'completed');

    const events = await getEvents(page, id);
    expect(events.length).toBeGreaterThan(0);
  });
});
