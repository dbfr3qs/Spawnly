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
// explicitly or they leak and starve the shared cluster. One agent listing is
// enough to resolve every subtree (DELETE doesn't cascade); children are
// deleted before their parents.
export async function killTrees(page: Page, rootIds: Iterable<string>): Promise<void> {
  let agents: AgentSummary[] = [];
  try {
    agents = await listAgents(page);
  } catch {
    /* fall back to deleting just the roots */
  }
  for (const rootId of rootIds) {
    const ids = [rootId, ...descendants(rootId, agents).map((a) => a.agentId)];
    // Leaf-first.
    for (const id of ids.reverse()) {
      await killAgent(page, id);
    }
  }
}
