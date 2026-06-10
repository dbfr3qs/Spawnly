import { test as teardown } from '@playwright/test';
import { listAgents } from '../helpers/dashboard';

// Runs once after the whole suite (wired as the `setup` project's teardown in
// playwright.config.ts). Sweeps every agent currently in the registry so the
// dashboard is left clean:
//   - DELETE removes the workload/pod (it does NOT cascade, so we iterate the
//     full flat list, which already includes children).
//   - dismiss flips the registry record's Dismissed flag, which is what hides it
//     from the dashboard (listAgents filters dismissed records out). DELETE
//     alone leaves the record visible, which is why agents lingered before.
// Best-effort: never throw, so a slow/again-spawned agent can't fail the run.
teardown('remove all agents', async ({ page }) => {
  let agents = [];
  try {
    agents = await listAgents(page);
  } catch {
    return;
  }
  for (const a of agents) {
    await page.request.delete(`/api/agents/${a.agentId}`).catch(() => {});
    await page.request.post(`/api/agents/${a.agentId}/dismiss`).catch(() => {});
  }
});
