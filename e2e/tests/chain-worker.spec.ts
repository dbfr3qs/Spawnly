import { test, expect } from '@playwright/test';
import { spawn, listAgents, descendants, type AgentSummary } from '../helpers/dashboard';
import { waitForEventType, newestEventTime } from '../helpers/events';
import { killTrees } from '../helpers/cleanup';

// Scenario 2 — cascading revoke/resume across an agent chain.
//
// chain-worker spawns one child of its own type up to maxDepth (4), forming a
// linear chain whose nodes each call the sample API every ~3s, emitting
// `work_ok`. Revoking a node cascades a permission denial to its whole subtree
// (it + all descendants), flipping them to `work_denied` while ancestors keep
// passing. Resuming reverses it.
//
// Actions (spawn / revoke / resume) go through the dashboard UI; event-state
// assertions read the timeline API (the same data the page polls) so they don't
// race the UI's re-render loop.

const sortedIds = (xs: Iterable<string>) => [...xs].sort();

test.describe('chain-worker', () => {
  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    // DELETE does not cascade to chain children, so remove each spawned root's
    // whole subtree.
    await killTrees(page, spawned.splice(0));
  });

  test('revoke/resume cascades down the chain', async ({ page }) => {
    test.setTimeout(300_000);
    await page.goto('/');

    const rootId = await spawn(page, 'chain-worker');
    spawned.push(rootId);

    // Wait for the chain to grow AND settle: poll until the node count is both
    // ≥3 and unchanged across two consecutive reads. A fixed sleep would race a
    // child still being scheduled — if one appears under the victim between our
    // snapshot and the revoke, the platform's cascade would be larger than the
    // tree we computed and the equality assertion below would flake. `chain`
    // holds the settled snapshot once the poll passes.
    let chain: AgentSummary[] = [];
    let prevLen = -1;
    await expect
      .poll(
        async () => {
          const all = await listAgents(page);
          const root = all.find((a) => a.agentId === rootId);
          chain = root ? [root, ...descendants(rootId, all)] : [];
          const stable = chain.length >= 3 && chain.length === prevLen;
          prevLen = chain.length;
          return stable;
        },
        { timeout: 150_000, intervals: [2000, 3000, 5000] },
      )
      .toBe(true);
    const chainIds = chain.map((a) => a.agentId);
    console.log(`chain (${chain.length}):`, chainIds.join(' → '));

    // 1) Every node is doing work.
    for (const node of chain) {
      await waitForEventType(page, node.agentId, 'work_ok', { timeout: 40_000 });
    }

    // 2) Revoke a random non-root child. Its subtree (itself + descendants)
    //    should be denied; everything shallower should keep working.
    const nonRoot = chain.filter((a) => a.agentId !== rootId);
    const victim = nonRoot[Math.floor(Math.random() * nonRoot.length)];
    const victimSubtree = [victim, ...descendants(victim.agentId, chain)];
    const victimIds = new Set(victimSubtree.map((a) => a.agentId));
    const survivors = chain.filter((a) => !victimIds.has(a.agentId));
    console.log('revoking:', victim.agentId, '→ subtree', sortedIds(victimIds).join(', '));

    // Baseline timestamps so assertions only count events produced after the
    // revoke (old work_ok rows never satisfy the work_denied waits, and vice versa).
    const sinceRevoke: Record<string, number> = {};
    for (const node of chain) sinceRevoke[node.agentId] = await newestEventTime(page, node.agentId);

    const [revokeResp] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().includes(`/api/agents/${victim.agentId}/revoke`) && r.request().method() === 'POST',
      ),
      page.locator(`[id="revokewrap-${victim.agentId}"] [data-testid="revoke"]`).click(),
    ]);
    const revoked: string[] = (await revokeResp.json()).revoked ?? [];
    // Cross-check the system's reported cascade against the tree we computed.
    expect(sortedIds(revoked)).toEqual(sortedIds(victimIds));

    // Behavioural proof: subtree flips to work_denied…
    for (const node of victimSubtree) {
      await waitForEventType(page, node.agentId, 'work_denied', {
        since: sinceRevoke[node.agentId],
        timeout: 40_000,
      });
    }
    // …and the rest keep working (cascade is scoped).
    for (const node of survivors) {
      await waitForEventType(page, node.agentId, 'work_ok', {
        since: sinceRevoke[node.agentId],
        timeout: 40_000,
      });
    }

    // 3) Resume the same node; its subtree returns to work_ok.
    const resumeBtn = page.locator(`[id="revokewrap-${victim.agentId}"] [data-testid="resume"]`);
    await expect(resumeBtn).toBeVisible({ timeout: 15_000 });

    const sinceResume: Record<string, number> = {};
    for (const node of victimSubtree) sinceResume[node.agentId] = await newestEventTime(page, node.agentId);

    const [resumeResp] = await Promise.all([
      page.waitForResponse(
        (r) => r.url().includes(`/api/agents/${victim.agentId}/resume`) && r.request().method() === 'POST',
      ),
      resumeBtn.click(),
    ]);
    const resumed: string[] = (await resumeResp.json()).resumed ?? [];
    expect(sortedIds(resumed)).toEqual(sortedIds(victimIds));

    for (const node of victimSubtree) {
      await waitForEventType(page, node.agentId, 'work_ok', {
        since: sinceResume[node.agentId],
        timeout: 40_000,
      });
    }
  });
});
