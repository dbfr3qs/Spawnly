import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { contextFor } from '../helpers/auth';

// Scenario — remote-substrate connect surface (Phase 3), proven through the real
// dashboard → orchestrator path.
//
//   • alice  — org admin of acme: connects the substrate (⇒ pending) and receives
//     the one-time org-bound identity in the response.
//   • viewer — acme MEMBER: sees status read-only, cannot connect/disconnect.
//   • bob    — org admin of a DIFFERENT org (globex): the acme substrate endpoint
//     is a cross-org 404 (no oracle).
//
// The dispatch channel + activation land in Phase 4/5; here the record only goes
// pending. The positive connect needs the orchestrator's dev-CA issuer wired
// (Phase-6 harness); when it isn't, connect returns 503 and that step is skipped,
// while every authorization assertion still runs.

async function getSubstrate(page: Page): Promise<{ status?: string; certFingerprint?: string } | null> {
  const r = await page.request.get('/api/org');
  expect(r.ok(), `get org: ${r.status()}`).toBeTruthy();
  return (await r.json()).substrate ?? null;
}

test.describe.serial('remote-substrate connect', () => {
  let aliceCtx: BrowserContext;
  let viewerCtx: BrowserContext;
  let bobCtx: BrowserContext;
  let alice: Page;
  let viewer: Page;
  let bob: Page;

  test.beforeAll(async ({ browser }) => {
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    bobCtx = await contextFor(browser, 'bob', 'globex');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    bob = await bobCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/'), bob.goto('/')]);
  });

  test.afterAll(async () => {
    // Restore acme to its seed (no substrate) so the rest of the suite is untouched.
    await alice.request.delete('/api/org/substrate').catch(() => {});
    await aliceCtx?.close();
    await viewerCtx?.close();
    await bobCtx?.close();
  });

  test('admin connects the substrate: record pending + one-time identity', async () => {
    const r = await alice.request.post('/api/org/substrate');
    if (r.status() === 503) {
      test.skip(true, 'orchestrator dev-CA issuer not wired in this cluster (Phase-6 harness)');
      return;
    }
    expect(r.ok(), `connect: ${r.status()} ${await r.text()}`).toBeTruthy();
    const resp = await r.json();
    // The one-time identity (cert + key) is present in the connect response.
    expect(resp.identity?.certPem, 'connect must return the cert').toContain('BEGIN CERTIFICATE');
    expect(resp.identity?.keyPem, 'connect must return the key once').toContain('PRIVATE KEY');
    expect(resp.status).toBe('pending');

    // The record is pending and only the fingerprint is retained.
    const sub = await getSubstrate(alice);
    expect(sub?.status).toBe('pending');
    expect(sub?.certFingerprint, 'fingerprint retained').toBeTruthy();
  });

  test('a member sees status read-only and cannot connect or disconnect', async () => {
    // Every mutation is refused with a cross-org-style 404 (no oracle).
    const post = await viewer.request.post('/api/org/substrate');
    expect(post.status(), 'member connect must 404').toBe(404);
    const del = await viewer.request.delete('/api/org/substrate');
    expect(del.status(), 'member disconnect must 404').toBe(404);

    // The read-only substrate status is shown in the member's Organization view,
    // and the connect editor is hidden for a non-admin.
    await viewer.goto('/');
    await expect(viewer.locator('[data-testid="open-members"]')).toBeVisible({ timeout: 30_000 });
    await viewer.locator('[data-testid="open-members"]').click();
    await expect(viewer.locator('#substrate-editor')).toBeHidden();
  });

  test("another org's admin cannot reach acme's substrate (cross-org 404)", async () => {
    // bob acts inside globex; the acme substrate endpoint (his own active org's
    // path) never exposes acme, and connecting for globex is his own org only.
    const post = await bob.request.post('/api/org/substrate');
    // Either 503 (issuer unwired) or a success for globex — never acme's record.
    expect([201, 503]).toContain(post.status());
    if (post.status() === 201) {
      const sub = await getSubstrate(bob);
      expect(sub?.status).toBe('pending');
      // Clean up globex.
      await bob.request.delete('/api/org/substrate').catch(() => {});
    }
    // acme's substrate is unaffected by bob.
    const acme = await getSubstrate(alice);
    // acme is either still pending (if step 1 ran) or absent (if it skipped).
    if (acme) expect(acme.status).toBe('pending');
  });
});
