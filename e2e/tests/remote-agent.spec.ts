import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { contextFor } from '../helpers/auth';
import { listAgents } from '../helpers/dashboard';
import { getEvents } from '../helpers/events';

// Scenario — consumer-hosted (remote) agents (multi-tenancy Phase 10, spec
// criteria 11 + 12), proven end-to-end against the in-cluster FAKE STS.
//
// A remote agent runs on CONSUMER compute (here: a bare in-cluster Pod with the
// real sidecar in ATTESTOR=aws-sts + AGENT_PLACEMENT=remote mode, NO AgentWorkload
// CR, NO SPIRE socket) and authenticates with the org's anchored STS identity.
//
//   • criterion 11 — an anchored-org (acme) remote agent registered by a member
//     (viewer) self-registers, MINTS Spawnly tokens (mint_ok events), appears in
//     the dashboard list, and after a dashboard REVOKE its next mint FAILS.
//   • criterion 12 — the SAME STS account presented for an agent recorded under a
//     DIFFERENT org (globex, anchored to another account) is refused end-to-end:
//     the record never goes active.
//
// Prereqs (scripts/deploy-fake-sts.sh == make e2e-anchor-setup): fake-sts +
// remote-demo images built/loaded, and BOTH registry and keycloak trust fake-sts.

const ACME_ACCOUNT = '111122223333';
const ACME_ROLE = `arn:aws:sts::${ACME_ACCOUNT}:assumed-role/acme-agents/*`;
const GLOBEX_ACCOUNT = '444455556666';
const GLOBEX_ROLE = `arn:aws:sts::${GLOBEX_ACCOUNT}:assumed-role/globex-agents/*`;

function fakeCredential(principal: string): string {
  const pr = { method: 'GET', url: `http://fake-sts:8080/?Action=GetCallerIdentity&X-Fake-Principal=${principal}` };
  return Buffer.from(JSON.stringify(pr)).toString('base64');
}

// ensureAnchor registers + activates an org's STS anchor via its admin session,
// idempotently (re-register resets to pending, then activate). Leaves it active.
async function ensureAnchor(admin: Page, account: string, role: string, principal: string) {
  const put = await admin.request.put('/api/org/anchor', { data: { type: 'sts', awsAccountId: account, roleArnPattern: role } });
  expect(put.ok(), `register anchor: ${put.status()} ${await put.text()}`).toBeTruthy();
  const act = await admin.request.post('/api/org/anchor/activate', { data: { credential: fakeCredential(principal) } });
  expect(act.ok(), `activate anchor: ${act.status()} ${await act.text()}`).toBeTruthy();
}

function runRemotePod(pod: string, agentId: string, principal: string) {
  execFileSync('bash', ['../scripts/run-remote-agent.sh', pod, agentId, principal], { stdio: 'inherit' });
}
function deletePod(pod: string) {
  try {
    execFileSync('kubectl', ['delete', 'pod', pod, '--ignore-not-found', '--wait=false'], { stdio: 'ignore' });
  } catch {
    /* best-effort */
  }
}

async function statusOf(page: Page, agentId: string): Promise<string> {
  return (await listAgents(page)).find((a) => a.agentId === agentId)?.status ?? '';
}
async function hasEvent(page: Page, agentId: string, type: string): Promise<boolean> {
  return (await getEvents(page, agentId)).some((e) => e.type === type);
}

test.describe.serial('consumer-hosted (remote) agents', () => {
  let aliceCtx: BrowserContext, viewerCtx: BrowserContext, bobCtx: BrowserContext;
  let alice: Page, viewer: Page, bob: Page;
  const pods: string[] = [];

  test.beforeAll(async ({ browser }) => {
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    bobCtx = await contextFor(browser, 'bob', 'globex');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    bob = await bobCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/'), bob.goto('/')]);
    // Both orgs get an ACTIVE anchor (acme = its own account, globex = a DIFFERENT
    // account) so registration is allowed in both and cross-org isolation is real.
    await ensureAnchor(alice, ACME_ACCOUNT, ACME_ROLE, 'acme-good');
    await ensureAnchor(bob, GLOBEX_ACCOUNT, GLOBEX_ROLE, 'globex-good');
  });

  test.afterAll(async () => {
    for (const p of pods) deletePod(p);
    // Restore both orgs to seed (no anchor) so other suites are untouched.
    await alice.request.delete('/api/org/anchor').catch(() => {});
    await bob.request.delete('/api/org/anchor').catch(() => {});
    await aliceCtx?.close();
    await viewerCtx?.close();
    await bobCtx?.close();
  });

  test('anchored-org remote agent mints, lists, and revoke severs its next mint', async () => {
    test.setTimeout(240_000);
    // 1. A member (viewer) registers the remote agent into acme.
    const reg = await viewer.request.post('/api/org/agents', { data: { agentType: 'weather-monitor' } });
    expect(reg.ok(), `register remote: ${reg.status()} ${await reg.text()}`).toBeTruthy();
    const agentId = (await reg.json()).agentId as string;
    expect(agentId).toMatch(/^remote-/);

    // 2. Launch the "consumer" Pod (sidecar aws-sts/remote + demo) with acme's identity.
    const pod = `remote-acme-${agentId}`;
    pods.push(pod);
    runRemotePod(pod, agentId, 'acme-good');

    // 3. It self-registers through the org anchor → the record flips active.
    await expect.poll(() => statusOf(viewer, agentId), { timeout: 120_000, intervals: [2000, 3000, 5000] }).toBe('active');

    // 4. It MINTS Spawnly tokens through keycloak's org-aware verifier (criterion 11 mint half).
    await expect.poll(() => hasEvent(viewer, agentId, 'mint_ok'), { timeout: 60_000, intervals: [2000, 3000] }).toBe(true);

    // 5. The org admin (alice) REVOKES from the dashboard → SpiceDB severs it and
    //    the agent's next mint FAILS (criterion 11 lifecycle-parity half).
    const rev = await alice.request.post(`/api/agents/${agentId}/revoke`);
    expect(rev.ok(), `revoke: ${rev.status()} ${await rev.text()}`).toBeTruthy();
    await expect.poll(() => hasEvent(alice, agentId, 'mint_failed'), { timeout: 90_000, intervals: [2000, 3000, 5000] }).toBe(true);
  });

  test('a cross-org STS identity cannot authenticate an agent recorded under another org', async () => {
    test.setTimeout(180_000);
    // Register a remote agent under GLOBEX (as its member bob).
    const reg = await bob.request.post('/api/org/agents', { data: { agentType: 'weather-monitor' } });
    expect(reg.ok(), `register globex remote: ${reg.status()} ${await reg.text()}`).toBeTruthy();
    const agentId = (await reg.json()).agentId as string;

    // Launch a Pod presenting ACME's identity (account 111122223333) for this
    // GLOBEX-recorded agent. The attested account != globex's anchor account, so
    // the verifier refuses: the record must NEVER go active.
    const pod = `remote-iso-${agentId}`;
    pods.push(pod);
    runRemotePod(pod, agentId, 'acme-good');

    // Give the sidecar ample time to try (and keep failing) self-registration.
    await new Promise((r) => setTimeout(r, 30_000));
    expect(await statusOf(bob, agentId)).not.toBe('active');
    // And it certainly never minted.
    expect(await hasEvent(bob, agentId, 'mint_ok')).toBe(false);
  });
});
