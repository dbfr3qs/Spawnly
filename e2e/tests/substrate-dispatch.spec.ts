import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { contextFor } from '../helpers/auth';
import { listAgents } from '../helpers/dashboard';
import { getEvents } from '../helpers/events';

// Scenario — remote-substrate DISPATCH (Phase 4, spec AC 3): a member spawn in a
// substrate-connected org runs on the SUBSTRATE, not the platform.
//
//   • alice (acme org admin) connects the substrate; once its dispatch channel is
//     live the record flips to `active`.
//   • viewer (acme member) spawns a global agent. It must materialize as an
//     AgentWorkload in the `substrate-sim` namespace — NOT in `default` — and the
//     agent must self-register (record active) and mint a Keycloak token.
//
// This is the full LIVE positive path. It needs the local-sim harness wired:
//   1. `make substrate-e2e-setup` (dev CA + x509pop + the substrate bundle incl. the
//      substrate-agent), and
//   2. the orchestrator's CP-mediated Entry API reachable (SPIRE_SERVER_SOCKET
//      delegated-admin path).
// When either isn't wired in this cluster (the common CI case), the substrate
// never activates and this spec SKIPS gracefully — the connect-surface + Go/unit
// isolation proofs cover the rest. (See plans/remote-substrate.)

const NS = 'substrate-sim';

function kubectl(args: string[]): string {
  try {
    return execFileSync('kubectl', args, { encoding: 'utf8' });
  } catch {
    return '';
  }
}

// simUp reports whether the substrate bundle (agent) is present in the cluster.
function simUp(): boolean {
  const out = kubectl(['get', 'deployment', 'substrate-agent', '-n', NS, '--ignore-not-found', '-o', 'name']);
  return out.includes('substrate-agent');
}

// agentWorkloadNamespace returns the namespace an AgentWorkload with `name` lives
// in, or '' if it exists nowhere.
function agentWorkloadIn(ns: string, name: string): boolean {
  const out = kubectl(['get', 'agentworkload', name, '-n', ns, '--ignore-not-found', '-o', 'name']);
  return out.trim().length > 0;
}

async function substrateStatus(admin: Page): Promise<string> {
  const r = await admin.request.get('/api/org');
  if (!r.ok()) return '';
  return (await r.json()).substrate?.status ?? '';
}

test.describe.serial('remote-substrate dispatch', () => {
  let aliceCtx: BrowserContext;
  let viewerCtx: BrowserContext;
  let alice: Page;
  let viewer: Page;

  test.beforeAll(async ({ browser }) => {
    test.skip(!simUp(), 'substrate sim not installed — run `make substrate-e2e-setup` first');
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/')]);
  });

  test.afterAll(async () => {
    // Deliberately does NOT disconnect the substrate: the channel is shared state
    // owned by the substrate-setup project and torn down by global.teardown's
    // sweepSubstrate. Disconnecting here dropped the channel for every substrate
    // suite that runs after this file (alphabetical order, single worker).
    await aliceCtx?.close();
    await viewerCtx?.close();
  });

  test('a member spawn runs on the substrate, not the platform', async () => {
    // 1./2. The substrate-setup project (tests/substrate.setup.ts) owns connecting
    // and bringing the channel online — assert it, don't redo it. Re-connecting
    // here ROTATES the org identity, which invalidates the leaf the substrate agent
    // is currently presenting and drops the very channel this test needs (it then
    // never re-activates, so the test silently skipped).
    let active = false;
    for (let i = 0; i < 30 && !active; i++) {
      active = (await substrateStatus(alice)) === 'active';
      if (!active) await new Promise((r) => setTimeout(r, 2_000));
    }
    test.skip(
      !active,
      'substrate channel not active — run `make substrate-e2e-setup` (the substrate-setup project brings it online)',
    );

    // 3. Member spawns. The spawn must be substrate-placed (no CR in default).
    // chain-worker is a requiresTenant type, so the spawn must carry a tenantId
    // (the API 400s otherwise) — same convention as spawn-tenant-field.spec.ts.
    const spawn = await viewer.request.post('/api/spawn', {
      data: { agentType: 'chain-worker', tenantId: 'tenant-42' },
    });
    expect(spawn.ok(), `spawn: ${spawn.status()} ${await spawn.text()}`).toBeTruthy();
    const workloadName: string = (await spawn.json()).workloadName;
    expect(workloadName).toBeTruthy();

    // 4. A local AgentWorkload appears in substrate-sim, and NONE in default.
    let onSubstrate = false;
    for (let i = 0; i < 30 && !onSubstrate; i++) {
      onSubstrate = agentWorkloadIn(NS, workloadName);
      if (!onSubstrate) await new Promise((r) => setTimeout(r, 2_000));
    }
    expect(onSubstrate, `AgentWorkload ${workloadName} should exist in ${NS}`).toBeTruthy();
    expect(
      agentWorkloadIn('default', workloadName),
      `AgentWorkload ${workloadName} must NOT exist in default (it runs on the substrate)`,
    ).toBeFalsy();

    // 5. The agent self-registers with its org-scoped SVID and mints a token —
    //    the record reaches active and a mint event lands (AC 3).
    let minted = false;
    for (let i = 0; i < 60 && !minted; i++) {
      const rec = (await listAgents(viewer)).find((a) => a.agentId === workloadName);
      const events = await getEvents(viewer, workloadName).catch(() => []);
      minted = rec?.status === 'active' || events.some((e) => e.type.includes('mint') || e.type.includes('token'));
      if (!minted) await new Promise((r) => setTimeout(r, 2_000));
    }
    expect(minted, 'substrate agent should self-register (active) and mint a Keycloak token').toBeTruthy();

    // 6. Child spawn lands on the SAME substrate (same org ⇒ same substrate; no
    //    per-spawn selector). chain-worker self-spawns, so a child MUST appear with
    //    parentId == workloadName; its AgentWorkload must also be in substrate-sim,
    //    never in default — completing the full substrate path (spec §2.6). HARD
    //    assert: a missing child is exactly the regression it guards (a child spawn
    //    silently not routed to the substrate). Child records appear before consent
    //    resolves (the CIBA grant runs in the child's own sidecar), so it is safe.
    let child = '';
    for (let i = 0; i < 60 && !child; i++) {
      child = (await listAgents(viewer)).find((a) => a.parentId === workloadName)?.agentId ?? '';
      if (!child) await new Promise((r) => setTimeout(r, 2_000));
    }
    expect(child, 'chain-worker child should appear (child spawns must reach the substrate)').toBeTruthy();
    let childOnSubstrate = false;
    for (let i = 0; i < 30 && !childOnSubstrate; i++) {
      childOnSubstrate = agentWorkloadIn(NS, child);
      if (!childOnSubstrate) await new Promise((r) => setTimeout(r, 2_000));
    }
    expect(childOnSubstrate, `child ${child} should run on the SAME substrate (${NS})`).toBeTruthy();
    expect(agentWorkloadIn('default', child), `child ${child} must NOT run on the platform`).toBeFalsy();
  });
});
