import { test as teardown } from '@playwright/test';
import { listAgents } from '../helpers/dashboard';
import { clearDemoAnchors, sweepOrgs, sweepRemotePods, sweepSubstrate } from '../helpers/cleanup';
import { contextFor } from '../helpers/auth';

// Runs once after the whole suite (wired as the `setup` project's teardown in
// playwright.config.ts). Sweeps the residue every kind of spec leaves behind so
// repeated `make e2e` runs on the same cluster stay isolated:
//   - remote-demo Pods (remote-agent.spec) — label-swept via kubectl.
//   - org-<suffix> orgs (signup.spec) — realm-admin DELETE cascade (Keycloak org
//     + SpiceDB tuples + registry record); the seeded demo orgs are excluded.
//   - demo-org attestation anchors (anchor/remote-agent specs) back to seed.
//   - substrate residue (substrate* specs): disconnect demo-org substrate records,
//     restore the substrate-agent replica, drop per-run proof/forged sim pods.
//   - every remaining agent: DELETE removes the workload/pod (it does NOT cascade,
//     so we iterate the full flat list, which already includes children); dismiss
//     flips the record's Dismissed flag, which is what hides it from the dashboard
//     (listAgents filters dismissed records out) — DELETE alone leaves it visible.
// Best-effort throughout: never throw, so a slow/again-spawned agent, a missing
// admin route, or an unreachable kubectl can't fail the run.
teardown('remove all agents and org/anchor/remote residue', async ({ page, browser }) => {
  sweepRemotePods();
  // org-<suffix> DELETE cascade is realm-admin gated — run it as root (the platform
  // master admin); the default `page` (alice) is only an org admin now.
  const rootCtx = await contextFor(browser, 'root');
  try {
    await sweepOrgs(await rootCtx.newPage());
  } finally {
    await rootCtx.close();
  }
  await clearDemoAnchors(browser);

  let agents = [];
  try {
    agents = await listAgents(page);
  } catch {
    agents = [];
  }
  for (const a of agents) {
    await page.request.delete(`/api/agents/${a.agentId}`).catch(() => {});
    await page.request.post(`/api/agents/${a.agentId}/dismiss`).catch(() => {});
  }
  // Substrate sweep runs LAST: disconnecting a demo-org substrate drops its
  // dispatch channel, so it must happen AFTER the agent DELETEs above (else a
  // substrate-placed agent's teardown can't be delivered and its CR/pod lingers).
  await sweepSubstrate(browser);
});
