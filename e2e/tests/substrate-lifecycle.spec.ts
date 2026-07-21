import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { contextFor } from '../helpers/auth';
import { listAgents } from '../helpers/dashboard';

// Scenario — remote-substrate LIFECYCLE PARITY + LIVENESS (Phase 5, spec AC 7/8):
//
//   • Lifecycle parity (AC 7): a dashboard REVOKE severs the substrate agent's
//     next mint (record → revoked); RESUME restores it; KILL/DELETE removes the
//     local pod/CR on the substrate.
//   • Disconnect semantics (AC 8): dropping the channel (scaling the
//     substrate-agent to 0) flips the substrate + its agents OFFLINE within a
//     bounded time WITHOUT mutating any record; reconnect restores live state. The
//     operator never terminally sweeps a substrate record.
//
// Like the Phase-4 dispatch spec this needs the local-sim harness wired
// (`make substrate-e2e-setup`, which wires the CP Entry API/delegated-admin socket). When that
// isn't present (the common CI case) the substrate never activates and the spec
// SKIPS gracefully — the Go/unit proofs (reaper kill routing, offline→terminal,
// liveness transitions, stale-sweep exclusion) cover the invariants headlessly.
// (See plans/remote-substrate.)

const NS = 'substrate-sim';

function kubectl(args: string[]): string {
  try {
    return execFileSync('kubectl', args, { encoding: 'utf8' });
  } catch {
    return '';
  }
}

function simUp(): boolean {
  return kubectl(['get', 'deployment', 'substrate-agent', '-n', NS, '--ignore-not-found', '-o', 'name']).includes(
    'substrate-agent',
  );
}

function agentWorkloadIn(ns: string, name: string): boolean {
  return kubectl(['get', 'agentworkload', name, '-n', ns, '--ignore-not-found', '-o', 'name']).trim().length > 0;
}

// spireEntryExists reports whether the hosted SPIRE server still holds an org-scoped
// registration entry whose SPIFFE ID carries `agentId` (its last path segment). The
// substrate agent creates this entry via the CP Entry API on spawn and MUST delete
// it on kill (Phase 5) — so this is the live proof of the entry-gone-on-kill
// invariant that was previously only unit-proven. Best-effort: a kubectl/exec
// failure returns false (treated as "gone") rather than throwing.
function spireEntryExists(agentId: string): boolean {
  const out = kubectl([
    'exec', '-n', 'spire-system', 'spire-server-0', '-c', 'spire-server', '--',
    '/opt/spire/bin/spire-server', 'entry', 'show',
  ]);
  // Entries print "SPIFFE ID : spiffe://<td>/org/<org>/agent/.../<agentId>".
  return new RegExp(`SPIFFE ID\\s*:\\s*spiffe://[^\\n]*/org/[^\\n]*/${agentId}(\\s|$)`).test(out);
}

function scaleSubstrateAgent(replicas: number): void {
  kubectl(['scale', 'deployment', 'substrate-agent', '-n', NS, `--replicas=${replicas}`]);
}

async function substrateRecordStatus(admin: Page): Promise<string> {
  const r = await admin.request.get('/api/org');
  if (!r.ok()) return '';
  return (await r.json()).substrate?.status ?? '';
}

async function substrateOnline(admin: Page): Promise<boolean> {
  const r = await admin.request.get('/api/org/substrate');
  if (!r.ok()) return false;
  return (await r.json()).online === true;
}

async function waitUntil(pred: () => Promise<boolean>, tries = 45, delayMs = 2_000): Promise<boolean> {
  for (let i = 0; i < tries; i++) {
    if (await pred()) return true;
    await new Promise((r) => setTimeout(r, delayMs));
  }
  return pred();
}

test.describe.serial('remote-substrate lifecycle + liveness', () => {
  let aliceCtx: BrowserContext;
  let viewerCtx: BrowserContext;
  let alice: Page; // acme org admin
  let viewer: Page; // acme member
  let workloadName = '';

  test.beforeAll(async ({ browser }) => {
    test.skip(!simUp(), 'substrate sim not installed — run `make substrate-e2e-setup` first');
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/')]);

    // The substrate-setup project (tests/substrate.setup.ts) owns connecting and
    // bringing the channel online. Do NOT re-connect here: the connect endpoint
    // ROTATES the org identity, which invalidates the leaf the substrate agent is
    // currently presenting and would drop the very channel this suite needs.
    const active = await waitUntil(async () => (await substrateRecordStatus(alice)) === 'active', 30);
    test.skip(
      !active,
      'substrate channel not active — run `make substrate-e2e-setup` (the substrate-setup project brings it online)',
    );

    // Spawn a long-lived agent onto the substrate to exercise its lifecycle.
    // chain-worker is a requiresTenant type — the spawn must carry a tenantId.
    const spawn = await viewer.request.post('/api/spawn', {
      data: { agentType: 'chain-worker', tenantId: 'tenant-42' },
    });
    expect(spawn.ok(), `spawn: ${spawn.status()} ${await spawn.text()}`).toBeTruthy();
    workloadName = (await spawn.json()).workloadName;
    expect(await waitUntil(() => Promise.resolve(agentWorkloadIn(NS, workloadName)), 30)).toBeTruthy();
  });

  test.afterAll(async () => {
    scaleSubstrateAgent(1); // never leave the sim scaled down
    // Deliberately does NOT disconnect the substrate: the channel is shared state
    // owned by the substrate-setup project and torn down by global.teardown's
    // sweepSubstrate. Disconnecting here would break whichever substrate suite runs
    // next (files run alphabetically under a single worker).
    await aliceCtx?.close();
    await viewerCtx?.close();
  });

  test('revoke severs the next mint; resume restores it (AC 7)', async () => {
    const revoke = await viewer.request.post(`/api/agents/${workloadName}/revoke`);
    expect(revoke.ok(), `revoke: ${revoke.status()}`).toBeTruthy();
    // Enforcement is CP-side at mint; the observable proxy is the record flipping
    // revoked (the substrate agent's next mint is refused by the verifiers/record).
    expect(
      await waitUntil(async () => (await listAgents(viewer)).find((a) => a.agentId === workloadName)?.status === 'revoked'),
    ).toBeTruthy();

    const resume = await viewer.request.post(`/api/agents/${workloadName}/resume`);
    expect(resume.ok(), `resume: ${resume.status()}`).toBeTruthy();
    expect(
      await waitUntil(async () => {
        const s = (await listAgents(viewer)).find((a) => a.agentId === workloadName)?.status;
        return s === 'active' || s === 'pending';
      }),
    ).toBeTruthy();
  });

  test('disconnect flips the substrate + agents offline without mutating records; reconnect restores (AC 8)', async () => {
    expect(await substrateOnline(alice), 'substrate should be online before the drop').toBeTruthy();

    // Drop the channel by scaling the substrate-agent to 0.
    scaleSubstrateAgent(0);
    expect(await waitUntil(async () => !(await substrateOnline(alice)))).toBeTruthy();

    // AC 8: the record is NOT mutated on disconnect — it stays `active`, and the
    // agent record is not swept to a terminal state.
    expect(await substrateRecordStatus(alice)).toBe('active');
    const rec = (await listAgents(viewer)).find((a) => a.agentId === workloadName);
    expect(['active', 'pending', 'revoked']).toContain(rec?.status);

    // Reconnect: the substrate-agent comes back and the channel re-activates.
    scaleSubstrateAgent(1);
    expect(await waitUntil(() => substrateOnline(alice))).toBeTruthy();
  });

  test('kill removes the local pod/CR AND the org-scoped SPIRE entry on the substrate (AC 7)', async () => {
    expect(agentWorkloadIn(NS, workloadName), 'workload should exist before kill').toBeTruthy();
    // The org-scoped SPIRE registration entry exists while the workload is live
    // (created via the CP Entry API on spawn). If the SPIRE server exec isn't
    // reachable we can't prove the entry side live — note it and skip that half.
    const entryProvable = spireEntryExists(workloadName);

    const del = await viewer.request.delete(`/api/agents/${workloadName}`);
    expect(del.ok(), `delete: ${del.status()} ${await del.text()}`).toBeTruthy();

    // The substrate agent tears down the local CR over the channel; it must vanish.
    expect(await waitUntil(() => Promise.resolve(!agentWorkloadIn(NS, workloadName)), 30)).toBeTruthy();
    // And it was never on the platform.
    expect(agentWorkloadIn('default', workloadName)).toBeFalsy();

    // AC 7 (entry half): the org-scoped SPIRE entry must be deleted too — the live
    // proof of what was previously only unit-covered. Only assert it if the entry
    // was observably present pre-kill (so a non-reachable SPIRE exec never fails it).
    if (entryProvable) {
      expect(
        await waitUntil(() => Promise.resolve(!spireEntryExists(workloadName)), 30),
        `org-scoped SPIRE entry for ${workloadName} must be deleted on kill`,
      ).toBeTruthy();
    }
  });
});
