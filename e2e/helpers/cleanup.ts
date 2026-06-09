import { type Page } from '@playwright/test';
import { descendants, listAgents } from './dashboard';

// Best-effort delete of a single agent. Never throws — it runs in afterEach and
// must not mask the test's own result.
export async function killAgent(page: Page, agentId: string): Promise<void> {
  try {
    await page.request.delete(`/api/agents/${agentId}`);
  } catch {
    /* best-effort cleanup */
  }
}

// Delete an agent and its entire subtree. DELETE does NOT cascade to chain
// children (only revoke/resume do), so spawned children must be removed
// explicitly or they leak and starve the shared cluster. Deletes children
// before parents.
export async function killTree(page: Page, rootId: string): Promise<void> {
  let agents = [];
  try {
    agents = await listAgents(page);
  } catch {
    /* fall back to deleting just the root */
  }
  const ids = [rootId, ...descendants(rootId, agents).map((a) => a.agentId)];
  // Leaf-first.
  for (const id of ids.reverse()) {
    await killAgent(page, id);
  }
}

// Kill every tracked root (and its subtree). Specs collect spawned root ids and
// pass them here in afterEach.
export async function killTrees(page: Page, rootIds: Iterable<string>): Promise<void> {
  for (const id of rootIds) {
    await killTree(page, id);
  }
}
