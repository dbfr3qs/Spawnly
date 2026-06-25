import { test, expect } from '@playwright/test';
import { spawn, listAgents, findAgent } from '../helpers/dashboard';
import { waitForEventType } from '../helpers/events';
import { killTrees } from '../helpers/cleanup';
import {
  consentPrompts,
  listConsents,
  resolveConsentPrompt,
  revokeAllConsents,
} from '../helpers/consent';

// Scenario — CIBA spawn consent.
//
// chain-worker's template gates chain-worker children behind user consent
// (delegation.childPolicies). Spawning a root (a user action, parentless) needs
// no consent, but the root's first self-spawned link does: its sidecar opens a
// CIBA backchannel authentication request, the registry record sits in
// awaiting-consent, and the dashboard shows an approve/deny prompt for the
// logged-in user. Approval stores a consent for the (user, parent type, child
// type) edge so deeper links auto-approve server-side; revoking the stored
// consent forces the next spawn of the edge to re-prompt; denial fails the
// pending link and notifies its parent.

test.describe('ciba-consent', () => {
  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    await killTrees(page, spawned.splice(0));
  });

  test('consent gates the spawn edge: prompt, auto-approve, revoke, deny', async ({ page }) => {
    test.setTimeout(420_000);
    await page.goto('/');

    // Never consented before (consents persist on the shared cluster).
    await revokeAllConsents(page);

    const rootId = await spawn(page, 'chain-worker');
    spawned.push(rootId);

    // 1) The first link waits for the user: registry record awaiting-consent,
    //    prompt visible on the dashboard. Pin the prompt to the link's own
    //    agent id — an unscoped .first() could match a stale card from an
    //    earlier session and race ahead of this link's actual request.
    const link2 = await findAgent(page, (a) => a.parentId === rootId);
    const prompt = consentPrompts(page).and(
      page.locator(`[data-agent-id="${link2.agentId}"]`),
    );
    await expect(prompt).toBeVisible({ timeout: 150_000 });
    await expect(prompt).toContainText('chain-worker');
    await expect
      .poll(
        async () =>
          (await listAgents(page)).find((a) => a.agentId === link2.agentId)?.status,
        { timeout: 30_000, intervals: [1000, 2000] },
      )
      .toBe('awaiting-consent');

    // 2) Approve through the UI: the link activates and starts working.
    await resolveConsentPrompt(page, 'approve', { agentId: link2.agentId });
    await waitForEventType(page, link2.agentId, 'consent_granted', { timeout: 60_000 });
    await waitForEventType(page, link2.agentId, 'work_ok', { timeout: 90_000 });

    // 3) The next link auto-approves from the stored consent: it reaches
    //    work_ok without any prompt of ITS OWN ever appearing, and the registry
    //    still holds exactly one live consent for the edge. Scope the "no
    //    prompt" check to link3's card — a global empty-banner assertion flakes
    //    on any unrelated pending prompt (e.g. a leaked sibling chain's renewal
    //    re-consent) that has nothing to do with link3 auto-approving.
    const link3 = await findAgent(page, (a) => a.parentId === link2.agentId);
    await waitForEventType(page, link3.agentId, 'consent_granted', { timeout: 90_000 });
    await waitForEventType(page, link3.agentId, 'work_ok', { timeout: 90_000 });
    await expect(
      consentPrompts(page).and(page.locator(`[data-agent-id="${link3.agentId}"]`)),
    ).toHaveCount(0);

    const live = (await listConsents(page)).filter(
      (c) => c.parentType === 'chain-worker' && c.childType === 'chain-worker' && !c.revoked,
    );
    expect(live).toHaveLength(1);

    // Tear the first chain down before revoking, so any later prompt can only
    // belong to the second chain (a live link's token renewals would otherwise
    // surface their own re-consent requests in the banner).
    await killTrees(page, [rootId]);
    spawned.length = 0;

    // 4) Revoke the stored consent from the management modal.
    await page.locator('#open-consents-btn').click();
    // Scope to THIS test's chain-worker edge: the modal lists every consent the
    // user holds, so unrelated edges (e.g. leftover travel-planner consents on a
    // shared/persistent cluster) must not be counted or clicked. There is exactly
    // one chain-worker->chain-worker record (the edge is reused, not duplicated).
    const record = page
      .locator('[data-testid="consent-record"]')
      .filter({ hasText: 'chain-worker' });
    await expect(record).toHaveCount(1, { timeout: 15_000 });
    await record.locator('.btn-deny').click();
    await expect(record.locator('.consent-revoked-tag')).toBeVisible({ timeout: 15_000 });
    await page.locator('#close-consents-btn').click();

    // 5) A fresh chain re-prompts (the consent no longer applies) — deny it:
    //    the pending link fails and its parent is told.
    const root2 = await spawn(page, 'chain-worker');
    spawned.push(root2);

    // Target the new link's own prompt by agent id — a just-killed chain link's
    // renewal re-consent could share the banner for a few seconds.
    const deniedLink = await findAgent(page, (a) => a.parentId === root2);
    await resolveConsentPrompt(page, 'deny', { agentId: deniedLink.agentId, timeout: 150_000 });

    await waitForEventType(page, deniedLink.agentId, 'consent_denied', { timeout: 60_000 });
    await waitForEventType(page, root2, 'consent_denied', { timeout: 60_000 });
    await expect
      .poll(
        async () =>
          (await listAgents(page)).find((a) => a.agentId === deniedLink.agentId)?.status,
        { timeout: 90_000, intervals: [2000, 3000] },
      )
      .toBe('failed');

    // The parent is unaffected by the denial — it keeps doing its own work.
    await waitForEventType(page, root2, 'work_ok', { timeout: 60_000 });
  });
});
