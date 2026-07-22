import { type Browser, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { descendants, listAgents, type AgentSummary } from './dashboard';
import { contextFor } from './auth';

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

// The demo orgs are seeded by scripts/reseed-orgs.sh and must survive teardown;
// only per-run residue is swept below. Keep in sync with that seed.
const DEMO_ORGS = ['acme', 'globex'] as const;

// sweepOrgs best-effort deletes every org whose id matches `pattern`. The
// default targets the `org-<suffix>` slugs that signup.spec.ts creates per run
// and leaves behind (only the spawned agent is killed there) — repeated
// `make e2e` runs would otherwise accumulate orgs + their SpiceDB tuples. Needs
// a realm-admin session (the suite's cached alice storage state); the admin
// DELETE cascades the Keycloak org + SpiceDB tuples + registry record. The demo
// orgs never match `/^org-/`, so reseeded state is untouched. Never throws.
export async function sweepOrgs(page: Page, pattern: RegExp = /^org-/): Promise<void> {
  try {
    const r = await page.request.get('/api/admin/orgs');
    if (!r.ok()) return; // not a realm admin, or orchestrator hiccup — skip
    const orgs: Array<{ orgId?: string }> = await r.json();
    if (!Array.isArray(orgs)) return; // guard a future endpoint shape-change
    await Promise.all(
      orgs
        .map((o) => o?.orgId)
        .filter((id): id is string => !!id && pattern.test(id) && !DEMO_ORGS.includes(id as never))
        .map((id) => page.request.delete(`/api/admin/orgs/${encodeURIComponent(id)}`).catch(() => {})),
    );
  } catch {
    /* best-effort; never mask the suite's own result */
  }
}

// sweepRemotePods best-effort deletes the in-cluster Pods that remote-agent.spec
// launches to simulate consumer compute. They carry the `remote-demo=true` label
// (scripts/run-remote-agent.sh). The spec deletes its own pods in afterAll, but a
// mid-test failure (or a skipped afterAll) leaks them; sweeping by label here is
// the backstop. kubectl (not the browser) is the tool, mirroring the spec — the
// dashboard has no API for these bare, CR-less pods. Never throws.
export function sweepRemotePods(): void {
  try {
    execFileSync(
      'kubectl',
      ['delete', 'pod', '-l', 'remote-demo=true', '--ignore-not-found', '--wait=false'],
      { stdio: 'ignore' },
    );
  } catch {
    /* best-effort: kubectl absent or nothing to delete */
  }
}

// sweepSubstrate best-effort undoes the residue the remote-substrate specs
// (substrate / substrate-dispatch / substrate-lifecycle / substrate-isolation) can
// leave when a mid-test failure skips their afterAll: a still-connected demo-org
// substrate record, the substrate-agent scaled to 0 (a lifecycle-spec disconnect
// step that never restored), and the per-run proof/forged pods in substrate-sim.
// The org-B credential Secret is deliberately LEFT in place (deleting it would make
// the next run's isolation spec skip). Each spec restores its own state on success;
// this is the backstop so repeated `make e2e` runs on the same cluster stay
// isolated. Both demo orgs are swept (acme = active substrate, globex = isolation
// creds). Never throws.
export async function sweepSubstrate(browser: Browser): Promise<void> {
  // 1. Disconnect any lingering demo-org substrate record (org-admin session each).
  for (const [user, org] of [
    ['alice', 'acme'],
    ['bob', 'globex'],
  ] as const) {
    let ctx: Awaited<ReturnType<typeof contextFor>> | undefined;
    try {
      ctx = await contextFor(browser, user, org);
      await (await ctx.newPage()).request.delete('/api/org/substrate').catch(() => {});
    } catch {
      /* best-effort */
    } finally {
      await ctx?.close().catch(() => {});
    }
  }
  // 2. Never leave the substrate-agent scaled down (lifecycle spec's disconnect step)
  //    and drop the per-run proof/forged pods. kubectl is the tool — these are bare,
  //    CR-less sim objects the dashboard has no API for. The org-B credential Secret
  //    is intentionally kept (the next isolation run needs it).
  try {
    execFileSync('kubectl', ['scale', 'deployment', 'substrate-agent', '-n', 'substrate-sim', '--replicas=1'], { stdio: 'ignore' });
  } catch {
    /* sim not installed, or already at 1 */
  }
  try {
    execFileSync(
      'kubectl',
      ['delete', 'pod', 'substrate-proof', 'substrate-isolation-proof', 'substrate-forged-agent', '-n', 'substrate-sim', '--ignore-not-found', '--grace-period=0', '--wait=false'],
      { stdio: 'ignore' },
    );
  } catch {
    /* best-effort */
  }
  // 3. Remove every dispatched AgentWorkload CR (and its pod) left on the substrate.
  //    The disconnect in step 1 drops the channel, so any CR still present can no
  //    longer be torn down over it — and the CP sweeps the org's SPIRE entries on
  //    the next connect, which leaves those pods identity-less forever (Init:Error
  //    backoff). That residue outlives the run and poisons the NEXT one: a stuck
  //    pod looks exactly like a genuine dispatch failure. Deleting the CR lets the
  //    substrate operator garbage-collect the pod; the explicit pod delete covers
  //    an already-orphaned one. Runs LAST, after the disconnect, because a live
  //    channel would otherwise race these deletes with its own reconcile.
  for (const args of [
    ['delete', 'agentworkload', '--all', '-n', 'substrate-sim', '--ignore-not-found', '--wait=false'],
    ['delete', 'pod', '-l', 'agent-platform.io/managed=true', '-n', 'substrate-sim', '--ignore-not-found', '--grace-period=0', '--wait=false'],
  ]) {
    try {
      execFileSync('kubectl', args, { stdio: 'ignore' });
    } catch {
      /* best-effort: sim not installed, or nothing to delete */
    }
  }
}

// clearDemoAnchors best-effort resets the demo orgs' attestation anchors to their
// seed state (none). anchor.spec + remote-agent.spec each restore this in their
// own afterAll, but a beforeAll crash can skip that, and a lingering ACTIVE anchor
// would change the verifier-selection path for a later re-run of those specs. The
// anchor API is session-active-org scoped, so clearing needs an org-admin session
// per org (alice→acme, bob→globex). Fully wrapped: any login/API failure is
// swallowed so teardown never fails on it.
export async function clearDemoAnchors(browser: Browser): Promise<void> {
  for (const [user, org] of [
    ['alice', 'acme'],
    ['bob', 'globex'],
  ] as const) {
    let ctx: Awaited<ReturnType<typeof contextFor>> | undefined;
    try {
      ctx = await contextFor(browser, user, org);
      const page = await ctx.newPage();
      await page.request.delete('/api/org/anchor').catch(() => {});
    } catch {
      /* best-effort */
    } finally {
      await ctx?.close().catch(() => {});
    }
  }
}
