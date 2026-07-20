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
// (`make substrate-setup` + the CP Entry API/delegated-admin socket). When that
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
    test.skip(!simUp(), 'substrate sim not installed — run `make substrate-setup` first');
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/')]);

    // Connect the substrate and wait for it to activate; otherwise skip the suite.
    const connect = await alice.request.post('/api/org/substrate');
    if (connect.status() === 503) test.skip(true, 'orchestrator dev-CA issuer not wired in this cluster');
    expect(connect.ok(), `connect: ${connect.status()} ${await connect.text()}`).toBeTruthy();
    const active = await waitUntil(async () => (await substrateRecordStatus(alice)) === 'active', 30);
    test.skip(!active, 'substrate channel never activated (Entry API/admin socket not wired in this cluster)');

    // Spawn a long-lived agent onto the substrate to exercise its lifecycle.
    const spawn = await viewer.request.post('/api/spawn', { data: { agentType: 'chain-worker' } });
    expect(spawn.ok(), `spawn: ${spawn.status()} ${await spawn.text()}`).toBeTruthy();
    workloadName = (await spawn.json()).workloadName;
    expect(await waitUntil(() => Promise.resolve(agentWorkloadIn(NS, workloadName)), 30)).toBeTruthy();
  });

  test.afterAll(async () => {
    scaleSubstrateAgent(1); // never leave the sim scaled down
    await alice?.request.delete('/api/org/substrate').catch(() => {});
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

  test('kill removes the local pod/CR on the substrate (AC 7)', async () => {
    expect(agentWorkloadIn(NS, workloadName), 'workload should exist before kill').toBeTruthy();
    const del = await viewer.request.delete(`/api/agents/${workloadName}`);
    expect(del.ok(), `delete: ${del.status()} ${await del.text()}`).toBeTruthy();

    // The substrate agent tears down the local CR (and its org-scoped SPIRE entry)
    // over the channel; the CR must vanish from substrate-sim.
    expect(await waitUntil(() => Promise.resolve(!agentWorkloadIn(NS, workloadName)), 30)).toBeTruthy();
    // And it was never on the platform.
    expect(agentWorkloadIn('default', workloadName)).toBeFalsy();
  });
});
