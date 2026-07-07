import { test as teardown } from '@playwright/test';
import { listAgents } from '../helpers/dashboard';
import { clearDemoAnchors, sweepOrgs, sweepRemotePods } from '../helpers/cleanup';

// Runs once after the whole suite (wired as the `setup` project's teardown in
// playwright.config.ts). Sweeps the residue every kind of spec leaves behind so
// repeated `make e2e` runs on the same cluster stay isolated:
//   - remote-demo Pods (remote-agent.spec) — label-swept via kubectl.
//   - org-<suffix> orgs (signup.spec) — realm-admin DELETE cascade (Keycloak org
//     + SpiceDB tuples + registry record); the seeded demo orgs are excluded.
//   - demo-org attestation anchors (anchor/remote-agent specs) back to seed.
//   - every remaining agent: DELETE removes the workload/pod (it does NOT cascade,
//     so we iterate the full flat list, which already includes children); dismiss
//     flips the record's Dismissed flag, which is what hides it from the dashboard
//     (listAgents filters dismissed records out) — DELETE alone leaves it visible.
// Best-effort throughout: never throw, so a slow/again-spawned agent, a missing
// admin route, or an unreachable kubectl can't fail the run.
teardown('remove all agents and org/anchor/remote residue', async ({ page, browser }) => {
  sweepRemotePods();
  await sweepOrgs(page);
  await clearDemoAnchors(browser);

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
