import { test, expect } from '@playwright/test';
import { spawn, listAgents, findAgent } from '../helpers/dashboard';
import { waitForEventType } from '../helpers/events';
import { killTrees } from '../helpers/cleanup';
import { revokeAllConsents } from '../helpers/consent';
import {
  mobileAccessToken,
  gw,
  streamFirstConsentEvent,
  type GatewayConsentRequest,
} from '../helpers/mobile';

// Scenario — Mobile CIBA spawn consent (the gateway path).
//
// The same chain-worker consent edge as ciba-consent.spec.ts, but answered the
// way the mobile app does: a user access token from the public `mobile` PKCE
// client, the gateway's per-user SSE stream surfacing the pending prompt, and
// the gateway's /me/consent-requests/{id}/approve proxy resolving it. Proves the
// registry→gateway webhook→SSE delivery and the user-scoped consent proxy end to
// end, under `make bootstrap` (NOTIFIER=dev — no cloud push credentials).
//
// Requires: a bootstrapped cluster + the mobile-gateway deployed + its public
// port forwarded to localhost:8091 (scripts/e2e.sh does this).

test.describe('mobile-ciba', () => {
  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    await killTrees(page, spawned.splice(0));
  });

  test('gateway: SSE prompt + token-scoped approve activates the link', async ({ page }) => {
    test.setTimeout(420_000);
    await page.goto('/');
    await revokeAllConsents(page); // start from "never consented"

    const token = await mobileAccessToken(page);

    // Device registration is scoped to the token's user (smoke-checks /me/devices).
    const reg = await gw(page, token).post('/me/devices', {
      platform: 'android',
      pushToken: `e2e-token-${Date.now()}`,
    });
    expect(reg.status()).toBe(201);

    // Open the SSE stream BEFORE spawning so we don't miss the event.
    const eventPromise = streamFirstConsentEvent(token, 180_000);

    const rootId = await spawn(page, 'chain-worker');
    spawned.push(rootId);

    // The root's first self-spawned link needs consent → its sidecar opens the
    // CIBA request → the registry fires the notifier webhook → the gateway fans
    // a consent_pending event to alice's stream.
    const link = await findAgent(page, (a) => a.parentId === rootId);
    const event = await eventPromise;
    expect(event.childType).toBe('chain-worker');

    // The gateway's user-scoped list shows the same pending request.
    const listResp = await gw(page, token).get('/me/consent-requests?status=pending');
    expect(listResp.ok()).toBeTruthy();
    const pending: GatewayConsentRequest[] = await listResp.json();
    const mine = pending.find((c) => c.id === event.consentRequestId);
    expect(mine, 'the streamed request id is in the user-scoped pending list').toBeTruthy();

    // Detail fetch (the app re-fetches authoritative state, never trusts the push).
    const detail = await gw(page, token).get(`/me/consent-requests/${event.consentRequestId}`);
    expect(detail.ok()).toBeTruthy();

    // Approve through the gateway → orchestrator → registry. The link activates.
    const approve = await gw(page, token).post(
      `/me/consent-requests/${event.consentRequestId}/approve`,
    );
    expect(approve.ok()).toBeTruthy();

    await waitForEventType(page, link.agentId, 'consent_granted', { timeout: 90_000 });
    await expect
      .poll(
        async () => (await listAgents(page)).find((a) => a.agentId === link.agentId)?.status,
        { timeout: 90_000, intervals: [1000, 2000] },
      )
      .toBe('active');
  });
});
