import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { contextFor } from '../helpers/auth';

// Scenario — per-org attestation anchor lifecycle + proof of control (spec
// criterion 10), proven through the real dashboard → orchestrator → registry
// path against the in-cluster FAKE STS (deployed by scripts/deploy-fake-sts.sh,
// which also points the registry's STS_TRUSTED_ENDPOINT at it).
//
// acme has an org admin (alice) and a member (viewer). The fake STS is
// configured (deploy/manifests/fake-sts.yaml) with two principals:
//   • acme-good  → account 111122223333, role acme-agents   (the legit identity)
//   • acme-wrong → account 999988887777, role intruder       (a different account)
//
// Flow: alice registers the anchor (⇒ pending); activating with the WRONG-account
// identity fails (400) and the anchor stays pending; activating with the right
// account+role activates it; the member sees the anchor but cannot edit it.

const ACME_ACCOUNT = '111122223333';
const ACME_ROLE_PATTERN = `arn:aws:sts::${ACME_ACCOUNT}:assumed-role/acme-agents/*`;

// fakeCredential builds the base64 presigned-request credential the registry
// replays against the fake STS. The fake ignores the (absent) SigV4 signature
// and maps the X-Fake-Principal token to a configured account+ARN, so no real
// AWS is involved. The URL must point at the in-cluster fake (the registry's
// sole trusted STS endpoint) and invoke GetCallerIdentity.
function fakeCredential(principal: string): string {
  const pr = {
    method: 'GET',
    url: `http://fake-sts:8080/?Action=GetCallerIdentity&X-Fake-Principal=${principal}`,
  };
  return Buffer.from(JSON.stringify(pr)).toString('base64');
}

async function getAnchor(page: Page): Promise<{ status?: string; awsAccountId?: string } | null> {
  const r = await page.request.get('/api/org');
  expect(r.ok(), `get org: ${r.status()}`).toBeTruthy();
  return (await r.json()).anchor ?? null;
}

// Serial: the steps depend on each other (register → activate → member view).
test.describe.serial('per-org attestation anchor', () => {
  let aliceCtx: BrowserContext;
  let viewerCtx: BrowserContext;
  let alice: Page;
  let viewer: Page;

  test.beforeAll(async ({ browser }) => {
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/')]);
  });

  test.afterAll(async () => {
    // Restore acme to its seed (no anchor) so the rest of the suite is untouched.
    await alice.request.delete('/api/org/anchor').catch(() => {});
    await aliceCtx?.close();
    await viewerCtx?.close();
  });

  test('admin registers an anchor and it lands pending', async () => {
    const r = await alice.request.put('/api/org/anchor', {
      data: { type: 'sts', awsAccountId: ACME_ACCOUNT, roleArnPattern: ACME_ROLE_PATTERN },
    });
    expect(r.ok(), `register anchor: ${r.status()} ${await r.text()}`).toBeTruthy();
    const anchor = await getAnchor(alice);
    expect(anchor?.status).toBe('pending');
    expect(anchor?.awsAccountId).toBe(ACME_ACCOUNT);
  });

  test('activation from the WRONG account is refused and stays pending', async () => {
    const r = await alice.request.post('/api/org/anchor/activate', {
      data: { credential: fakeCredential('acme-wrong') },
    });
    expect(r.status(), 'wrong-account activation must be a generic 400').toBe(400);
    // Oracle-free: the body must not reveal WHY it failed.
    const body = await r.text();
    expect(body).not.toContain('account');
    expect(body).not.toContain('mismatch');
    expect((await getAnchor(alice))?.status).toBe('pending');
  });

  test('activation with the right account + role activates the anchor', async () => {
    const r = await alice.request.post('/api/org/anchor/activate', {
      data: { credential: fakeCredential('acme-good') },
    });
    expect(r.ok(), `activate: ${r.status()} ${await r.text()}`).toBeTruthy();
    await expect
      .poll(async () => (await getAnchor(alice))?.status ?? '', { timeout: 10_000 })
      .toBe('active');
  });

  test('a member sees the anchor but cannot register or activate it', async () => {
    // Visible read-only: the member's GET returns the (now active) anchor.
    const anchor = await getAnchor(viewer);
    expect(anchor?.status).toBe('active');
    expect(anchor?.awsAccountId).toBe(ACME_ACCOUNT);

    // But every mutation is refused with a cross-org-style 404 (no oracle).
    const put = await viewer.request.put('/api/org/anchor', {
      data: { type: 'sts', awsAccountId: '000011112222', roleArnPattern: 'arn:aws:sts::000011112222:assumed-role/x/*' },
    });
    expect(put.status(), 'member register must 404').toBe(404);
    const act = await viewer.request.post('/api/org/anchor/activate', {
      data: { credential: fakeCredential('acme-good') },
    });
    expect(act.status(), 'member activate must 404').toBe(404);
    const del = await viewer.request.delete('/api/org/anchor');
    expect(del.status(), 'member clear must 404').toBe(404);

    // The anchor is unchanged after the member's attempts.
    expect((await getAnchor(alice))?.status).toBe('active');

    // And the read-only anchor status is shown in the member's Organization view.
    await viewer.goto('/');
    await expect(viewer.locator('[data-testid="open-members"]')).toBeVisible({ timeout: 30_000 });
    await viewer.locator('[data-testid="open-members"]').click();
    await expect(viewer.locator('[data-testid="anchor-state"]')).toHaveText('active');
    // The editor (register/clear) is hidden for a non-admin.
    await expect(viewer.locator('#anchor-editor')).toBeHidden();
  });
});
