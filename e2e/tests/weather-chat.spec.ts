import { test, expect } from '@playwright/test';
import { spawn, waitForStatus } from '../helpers/dashboard';
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

    const chatBtn = page.locator(`[id="chatbtn-${id}"]`);
    await expect(chatBtn).toBeVisible({ timeout: 60_000 });
    await chatBtn.click();

    const input = page.locator(`[id="chatinput-${id}"]`);
    await expect(input).toBeVisible();
    await input.fill('Hi');
    await input.press('Enter');

    // The typing bubble shares .chat-msg.agent — exclude it to match the real
    // reply. LLM latency dominates, so allow a generous window.
    const reply = page.locator(`[id="chatlog-${id}"] .chat-msg.agent:not(.typing)`).first();
    await expect(reply).toBeVisible({ timeout: 90_000 });
    await expect(reply).not.toHaveText(/^\[Error:/);

    const text = (await reply.textContent())?.trim() ?? '';
    expect(text.length).toBeGreaterThan(0);
  });
});
