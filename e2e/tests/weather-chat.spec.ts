import { test, expect } from '@playwright/test';
import { spawn, waitForStatus, chat } from '../helpers/dashboard';
import { killTrees } from '../helpers/cleanup';

// Scenario 3 — chat. Spawn the weather-monitor (long-lived, supportsChat), open
// its chat panel, send "Hi", and verify a non-empty agent reply comes back.
// Depends on the cluster's configured AI provider (default: Anthropic — see .env).
test.describe('weather-monitor', () => {
  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    await killTrees(page, spawned.splice(0));
  });

  test('spawns, accepts a chat message, and replies', async ({ page }) => {
    await page.goto('/');

    const id = await spawn(page, 'weather-monitor');
    spawned.push(id);

    // Long-lived agent: wait until it's active before chatting so the message
    // endpoint can reach a ready pod.
    await waitForStatus(page, id, 'active');

    // Send "Hi" and assert a non-empty, non-error reply. chat() opens the panel,
    // excludes the typing bubble, and retries the transient "agent unreachable"
    // warm-up window. LLM latency dominates, so allow a generous window.
    const text = await chat(page, id, 'Hi', 90_000);
    expect(text.length).toBeGreaterThan(0);
  });
});
