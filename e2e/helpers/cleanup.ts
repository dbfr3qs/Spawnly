import { type Page } from '@playwright/test';
import { descendants, listAgents, type AgentSummary } from './dashboard';

// Best-effort delete of a single agent. Never throws — it runs in afterEach and
// must not mask the test's own result.
export async function killAgent(page: Page, agentId: string): Promise<void> {
  try {
    await page.request.delete(`/api/agents/${agentId}`);
  } catch {
    /* best-effort cleanup */
  }
}

// Delete every tracked root and its entire subtree. DELETE does NOT cascade to
// chain children (only revoke/resume do), so spawned children must be removed
// explicitly or they leak and starve the shared cluster.
//
// chain-worker self-spawns at EVERY level, so a single leaf-first pass over one
// snapshot loses the race: any node still alive keeps spawning, and a child
// born after the snapshot is never in our id list — it orphans, survives the
// run, and (once a later test revokes its consent) perpetually re-surfaces
// consent prompts that flake unrelated tests. So we sweep in a settle loop:
// delete the whole known subtree, re-list, and repeat until a fresh listing
// shows the subtree empty (or we exhaust a bounded number of passes). Deleting
// the spawners on each pass starves the chain until growth stops.
export async function killTrees(page: Page, rootIds: Iterable<string>): Promise<void> {
  const roots = [...rootIds];
  if (roots.length === 0) return;

  for (let attempt = 0; attempt < 8; attempt++) {
    let agents: AgentSummary[];
    try {
      agents = await listAgents(page);
    } catch {
      return; // best-effort; never mask the test's own result
    }

    const targets = new Set<string>();
    for (const rootId of roots) {
      if (agents.some((a) => a.agentId === rootId)) targets.add(rootId);
      for (const d of descendants(rootId, agents)) targets.add(d.agentId);
    }
    if (targets.size === 0) return; // subtree fully gone

    await Promise.all([...targets].map((id) => killAgent(page, id)));
    // Let any spawn that was in flight during the listing register, so the
    // next pass's listing sees the new child instead of leaking it.
    await page.waitForTimeout(500);
  }
}
