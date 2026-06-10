import { test, expect } from '@playwright/test';
import { spawn, listAgents, type AgentSummary } from '../helpers/dashboard';
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

// Poll until the predicate finds an agent; returns it.
async function findAgent(
  page: Parameters<typeof listAgents>[0],
  pred: (a: AgentSummary) => boolean,
  timeout = 120_000,
): Promise<AgentSummary> {
  let found: AgentSummary | undefined;
  await expect
    .poll(
      async () => {
        found = (await listAgents(page)).find(pred);
        return Boolean(found);
      },
      { timeout, intervals: [1000, 2000, 3000] },
    )
    .toBe(true);
  return found!;
}

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
    //    prompt visible on the dashboard.
    const prompt = consentPrompts(page).first();
    await expect(prompt).toBeVisible({ timeout: 150_000 });
    await expect(prompt).toContainText('chain-worker');

    const link2 = await findAgent(page, (a) => a.parentId === rootId);
    expect(link2.status).toBe('awaiting-consent');

    // 2) Approve through the UI: the link activates and starts working.
    await resolveConsentPrompt(page, 'approve');
    await waitForEventType(page, link2.agentId, 'consent_granted', { timeout: 60_000 });
    await waitForEventType(page, link2.agentId, 'work_ok', { timeout: 90_000 });

    // 3) The next link auto-approves from the stored consent: it reaches
    //    work_ok without any prompt ever appearing, and the registry still
    //    holds exactly one live consent for the edge.
    const link3 = await findAgent(page, (a) => a.parentId === link2.agentId);
    await waitForEventType(page, link3.agentId, 'consent_granted', { timeout: 90_000 });
    await waitForEventType(page, link3.agentId, 'work_ok', { timeout: 90_000 });
    await expect(consentPrompts(page)).toHaveCount(0);

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
    const record = page.locator('[data-testid="consent-record"]');
    await expect(record).toHaveCount(1, { timeout: 15_000 });
    await record.locator('.btn-deny').click();
    await expect(record.locator('.consent-revoked-tag')).toBeVisible({ timeout: 15_000 });
    await page.locator('#close-consents-btn').click();

    // 5) A fresh chain re-prompts (the consent no longer applies) — deny it:
    //    the pending link fails and its parent is told.
    const root2 = await spawn(page, 'chain-worker');
    spawned.push(root2);

    await expect(consentPrompts(page).first()).toBeVisible({ timeout: 150_000 });
    const deniedLink = await findAgent(page, (a) => a.parentId === root2);
    await resolveConsentPrompt(page, 'deny');

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
