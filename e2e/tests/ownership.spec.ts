import { test, expect } from '@playwright/test';
import { spawn, listAgents } from '../helpers/dashboard';
import { killAgent } from '../helpers/cleanup';

// Scenario — agent ownership scoping. Every per-agent API the dashboard exposes
// is scoped to the authenticated session user: the list shows only your agents,
// and act/read endpoints 404 for an agent you don't own. These assertions run
// through the REAL dashboard → orchestrator → registry path, so they cover the
// end-to-end wiring the per-component unit tests can't (esp. the dashboard's
// id-escaping that stops a crafted id from smuggling a different userId).
//
// The harness authenticates as a single user, so the cross-user *denial* case
// (user A cannot touch user B's agent) needs a second OIDC identity — see the
// test.fixme at the bottom for the exact approach. Unit tests already prove the
// registry/orchestrator 404 a non-owner at every layer.

test.describe('ownership scoping', () => {
  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    for (const id of spawned.splice(0)) await killAgent(page, id);
  });

  test('the list shows your own agent and a crafted id cannot smuggle a userId', async ({
    page,
  }) => {
    test.setTimeout(120_000);
    await page.goto('/');

    // Spawn an agent as the session user; it must appear in the scoped list.
    const id = await spawn(page, 'weather-monitor');
    spawned.push(id);
    await expect
      .poll(async () => (await listAgents(page)).some((a) => a.agentId === id), {
        timeout: 30_000,
      })
      .toBe(true);

    // The Phase-B regression: a crafted agent id that embeds a different
    // `?userId=` must NOT delete the agent. The dashboard PathEscapes the id, so
    // the orchestrator sees a bogus agent id (not `id`) plus the session user —
    // ownership fails, nothing is deleted. Without the escape, the smuggled
    // userId would win and the registry's ownership check would pass.
    const crafted = encodeURIComponent(`${id}?userId=someone-else`);
    const resp = await page.request.delete(`/api/agents/${crafted}`);
    expect(resp.status(), 'crafted-id delete must be rejected, not silently routed').toBe(404);

    // The agent is still there — the smuggle did not delete it.
    expect((await listAgents(page)).some((a) => a.agentId === id)).toBe(true);

    // A normal owner delete is accepted and tears the agent down. Kill marks the
    // record terminal but leaves it LISTED until it's dismissed (kill vs dismiss),
    // so assert it reaches a terminal status — not that it vanishes from the list.
    const ok = await page.request.delete(`/api/agents/${encodeURIComponent(id)}`);
    expect(ok.ok()).toBeTruthy();
    await expect
      .poll(
        async () => (await listAgents(page)).find((a) => a.agentId === id)?.status ?? 'gone',
        { timeout: 60_000 },
      )
      .toMatch(/completed|killed|failed|gone/);
  });

  // Cross-user denial: user A must get 404 (and no effect) acting on user B's
  // agent. Requires a SECOND authenticated identity, which the current single-
  // user harness doesn't provision. Approach when adding it:
  //   1. Provision a second dashboard user (a second `dashboard-user`-style
  //      credential / storageState in auth.setup.ts) and a second `browser.newContext`.
  //   2. As user B, spawn an agent; capture its id.
  //   3. As user A, assert GET /api/agents excludes B's id, and that
  //      DELETE/revoke/resume/dismiss/events/logs/message on B's id all return
  //      404 and B's agent survives.
  // Until then this is covered by the registry/orchestrator/dashboard unit tests.
  test.fixme('user A cannot see or act on user B’s agent (needs a 2nd identity)', async () => {});
});
