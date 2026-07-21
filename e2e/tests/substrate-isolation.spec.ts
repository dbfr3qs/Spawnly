import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { contextFor } from '../helpers/auth';
import { listAgents } from '../helpers/dashboard';
import { killTrees } from '../helpers/cleanup';

// ─────────────────────────────────────────────────────────────────────────────
// THE test that matters (remote-substrate Phase 6, spec AC 4/5/6 + AC 1): the
// two-org isolation matrix, proven LIVE against the harness that
// `make substrate-e2e-setup` stands up (org A=acme active substrate; org B=globex
// credentials for the negatives).
//
// Isolation rests on three enforcement points, each proven at the layer it lives:
//   • identity spine (SVID trust-domain+org, node→org binding, forged-CA reject) —
//     driven with REAL SVIDs by scripts/substrate-isolation.sh, invoked here. This
//     is the "Go integration where the browser can't reach" made live: the real
//     Phase-1 verifier accepts org A's SVID for an org-A record and REFUSES it for
//     an org-B record (AC 4/5), and a forged/untrusted-CA leaf never attests (AC 1).
//   • Entry API cross-org refusal (AC 6) + dispatch-channel own-org-only (AC 4) —
//     exhaustively Go/JUnit-proven (internal/substrate.TestEntryAPIAuthzMatrix,
//     cmd/orchestrator.TestHub_PushOwnOrgOnly, OrgAwareVerifier{,TokenExchange}Test)
//     AND exercised by the running substrate; asserted here at the human layer
//     (a cross-org admin can never see or drive another org's substrate/agents).
//   • the POSITIVE DIAGONAL — org A's own member spawn genuinely runs on org A's
//     substrate (no CR in default), so isolation isn't just "everything is denied".
//
// Needs the live harness: `make substrate-e2e-setup`. When it isn't wired (the
// common CI case) each block SKIPS gracefully — the Go/JUnit matrix carries the
// invariants headless. (See plans/remote-substrate.)
// ─────────────────────────────────────────────────────────────────────────────

const NS = 'substrate-sim';
const ORG_A = 'acme';
const ORG_B = 'globex';

function kubectl(args: string[]): string {
  try {
    return execFileSync('kubectl', args, { encoding: 'utf8' });
  } catch {
    return '';
  }
}

// simUp: the org-A substrate bundle (agent) is installed.
function simUp(): boolean {
  return kubectl(['get', 'deployment', 'substrate-agent', '-n', NS, '--ignore-not-found', '-o', 'name']).includes(
    'substrate-agent',
  );
}

// orgBCredsPresent: the harness issued org B's isolation credential Secret.
function orgBCredsPresent(): boolean {
  return kubectl(['get', 'secret', `substrate-identity-${ORG_B}`, '-n', NS, '--ignore-not-found', '-o', 'name']).trim().length > 0;
}

function agentWorkloadIn(ns: string, name: string): boolean {
  return kubectl(['get', 'agentworkload', name, '-n', ns, '--ignore-not-found', '-o', 'name']).trim().length > 0;
}

async function substrateStatus(admin: Page): Promise<string> {
  const r = await admin.request.get('/api/org');
  if (!r.ok()) return '';
  return (await r.json()).substrate?.status ?? '';
}

async function waitUntil(pred: () => Promise<boolean> | boolean, tries = 30, delayMs = 2_000): Promise<boolean> {
  for (let i = 0; i < tries; i++) {
    if (await pred()) return true;
    await new Promise((r) => setTimeout(r, delayMs));
  }
  return Promise.resolve(pred());
}

test.describe.serial('remote-substrate two-org isolation matrix', () => {
  let aliceCtx: BrowserContext; // acme org admin
  let viewerCtx: BrowserContext; // acme member
  let bobCtx: BrowserContext; // globex org admin
  let alice: Page;
  let viewer: Page;
  let bob: Page;

  test.beforeAll(async ({ browser }) => {
    test.skip(!simUp(), 'substrate sim not installed — run `make substrate-e2e-setup` first');
    aliceCtx = await contextFor(browser, 'alice', ORG_A);
    viewerCtx = await contextFor(browser, 'viewer', ORG_A);
    bobCtx = await contextFor(browser, 'bob', ORG_B);
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    bob = await bobCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/'), bob.goto('/')]);
  });

  test.afterAll(async () => {
    // Deliberately does NOT disconnect: the org-A channel is SHARED state owned by
    // the substrate-setup project and torn down by global.teardown's sweepSubstrate.
    // Disconnecting here dropped the channel for every substrate spec that runs
    // after this file (substrate-lifecycle), which is why they skipped in a full run.
    await aliceCtx?.close();
    await viewerCtx?.close();
    await bobCtx?.close();
  });

  // (a)/(c) + node→org binding, LIVE with real SVIDs. The script is the source of
  // truth for the identity spine; a non-zero exit is an isolation breach and fails
  // the test loudly (its stdout/stderr is surfaced on failure).
  test('identity spine: org-A SVID refused for an org-B record; forged CA never attests (AC 1/4/5)', () => {
    test.skip(!orgBCredsPresent(), 'org-B isolation credentials absent — run `make substrate-e2e-setup`');
    test.setTimeout(240_000);
    try {
      // Sync exec can't be interrupted by test.setTimeout — cap it below the test
      // budget so a wedged kubectl/network call fails the test instead of the worker.
      const out = execFileSync('bash', ['../scripts/substrate-isolation.sh', ORG_A, ORG_B], {
        encoding: 'utf8',
        timeout: 220_000,
      });
      expect(out).toContain('PASS: two-org isolation matrix holds');
    } catch (e: unknown) {
      const err = e as { stdout?: string; stderr?: string; message?: string };
      throw new Error(`substrate-isolation.sh failed:\n${err.stdout ?? ''}\n${err.stderr ?? err.message ?? ''}`);
    }
  });

  // (b)/(d) at the human layer: with org A's substrate ACTIVE, org B's admin can
  // neither observe nor drive it, and org A's own member spawn genuinely lands on
  // the substrate (the positive diagonal). Skips if the channel never activates
  // (Entry API/admin socket not wired), like the dispatch/lifecycle specs.
  test('active org-A substrate is invisible to org B; org-A member spawn runs on it (diagonal, AC 3/5)', async () => {
    // Use the channel the substrate-setup project brought online; do NOT connect
    // here. Connect ROTATES the org identity: it issues a new leaf (which only the
    // setup project installs into the substrate cluster) and sweeps the previous
    // node's entries, so the running substrate-agent — still presenting the old
    // leaf — can no longer attest. The channel then dropped and this very test
    // skipped on its own `active` guard, taking substrate-lifecycle down with it.
    const active = await waitUntil(async () => (await substrateStatus(alice)) === 'active', 30);
    test.skip(!active, 'substrate channel not active — run `make substrate-e2e-setup` (the substrate-setup project brings it online)');

    // Cross-org: org B's admin sees his OWN org's substrate state (here: none/his
    // own), NEVER org A's — the /api/org substrate is derived from the session org
    // server-side, so bob can't read acme's record through it.
    const bobOrg = await bob.request.get('/api/org');
    expect(bobOrg.ok()).toBeTruthy();
    const bobSub = (await bobOrg.json()).substrate ?? null;
    // globex has no substrate connected in this spec; crucially it is not acme's.
    if (bobSub) {
      expect(bobSub.status, 'globex must never surface acme substrate state').not.toBe('active');
    }

    // Positive diagonal: an org-A member spawn is substrate-placed (no CR in default).
    // chain-worker is a requiresTenant type — the spawn must carry a tenantId.
    const spawn = await viewer.request.post('/api/spawn', {
      data: { agentType: 'chain-worker', tenantId: 'tenant-42' },
    });
    expect(spawn.ok(), `spawn: ${spawn.status()} ${await spawn.text()}`).toBeTruthy();
    const workloadName: string = (await spawn.json()).workloadName;
    expect(workloadName).toBeTruthy();

    const onSubstrate = await waitUntil(() => agentWorkloadIn(NS, workloadName), 30);
    expect(onSubstrate, `AgentWorkload ${workloadName} should exist in ${NS}`).toBeTruthy();
    expect(
      agentWorkloadIn('default', workloadName),
      `AgentWorkload ${workloadName} must NOT exist in default (it runs on org A's substrate)`,
    ).toBeFalsy();

    // Cross-org visibility: bob (globex) never sees acme's substrate agent in his list.
    const bobSees = (await listAgents(bob)).some((a) => a.agentId === workloadName);
    expect(bobSees, "globex admin must not see acme's substrate agent").toBeFalsy();

    // Clean up the diagonal spawn. chain-worker self-spawns, so sweep the whole
    // subtree (DELETE doesn't cascade) to avoid leaking children onto the substrate.
    await killTrees(viewer, [workloadName]);
  });
});
