import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { contextFor } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';
import { cloneSampleTemplate, isTypeOffered } from '../helpers/templates';
import { killAgent } from '../helpers/cleanup';

// Scenario — org-scoped templates (spec criterion 7). A template CREATED via the
// dashboard by an ORG admin is stamped with that org and is visible + spawnable
// ONLY within it; it is invisible and unspawnable in every other org, regardless
// of its requiresTenant value. The seeded global catalog (weather-monitor) stays
// spawnable in both. Seed contract:
//   acme   = alice (org admin; also realm admin)
//   globex = bob   (org admin; NOT a realm admin — so a template he creates is
//                   stamped to globex, not global)
//
// bob (a pure org admin) is the template author here precisely because a REALM
// admin's create would be GLOBAL (realm-admin authority), which wouldn't exercise
// org scoping. The template carries requiresTenant=true to prove the cross-org
// invisibility holds regardless of the (orthogonal) tenant primitive.

async function waitForListedStatus(page: Page, agentId: string, re = /active|pending/i, timeout = 120_000) {
  await expect
    .poll(async () => (await listAgents(page)).find((a) => a.agentId === agentId)?.status ?? '', { timeout })
    .toMatch(re);
}

test.describe('org-scoped templates', () => {
  let aliceCtx: BrowserContext; // acme
  let bobCtx: BrowserContext; // globex
  let alice: Page;
  let bob: Page;
  const orgType = `globex-only-${Date.now()}`;

  test.beforeAll(async ({ browser }) => {
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    bobCtx = await contextFor(browser, 'bob', 'globex');
    alice = await aliceCtx.newPage();
    bob = await bobCtx.newPage();
    await Promise.all([alice.goto('/'), bob.goto('/')]);

    // bob (globex org admin) creates an org-scoped template. requiresTenant=true so
    // the "regardless of requiresTenant" clause is exercised.
    const tmpl = cloneSampleTemplate(orgType);
    tmpl.requiresTenant = true;
    const resp = await bob.request.post('/api/templates', { data: tmpl });
    expect(resp.ok(), `org admin create ${orgType}: ${resp.status()} ${await resp.text()}`).toBeTruthy();
  });

  test.afterAll(async () => {
    // Disable then delete the throwaway template as its org admin.
    await bob.request.patch(`/api/templates/${orgType}`, { data: { status: 'disabled' } }).catch(() => {});
    await bob.request.delete(`/api/templates/${orgType}`).catch(() => {});
    await aliceCtx?.close();
    await bobCtx?.close();
  });

  test('the template is visible + offered in its own org (globex)', async () => {
    // Reload so the spawn dropdown re-fetches /api/templates/spawn AFTER the
    // template was created in beforeAll (it is populated once per page load).
    await bob.reload();
    expect(await isTypeOffered(bob, orgType), 'author org should see the template').toBe(true);
  });

  test('the template is INVISIBLE and UNSPAWNABLE in another org (acme), regardless of requiresTenant', async () => {
    // Not offered in acme's spawn dropdown.
    await alice.reload();
    expect(await isTypeOffered(alice, orgType), 'a cross-org template must not appear in acme').toBe(false);

    // The real boundary: a direct spawn attempt is refused with the SAME
    // "unknown agent type" 400 a nonexistent type yields (no existence oracle) —
    // and NOT a 400 about a missing tenantId, proving the org gate fires first.
    const resp = await alice.request.post('/api/spawn', { data: { agentType: orgType, tenantId: 'x' } });
    expect(resp.status(), 'cross-org spawn must 400').toBe(400);
    expect(await resp.text()).toContain('unknown agent type');
  });

  test('the seeded GLOBAL catalog stays spawnable in both orgs', async () => {
    // Fresh page so the modal/dropdown state is clean (prior tests left a modal open).
    await Promise.all([bob.reload(), alice.reload()]);
    const globexAgent = await spawn(bob, 'weather-monitor');
    const acmeAgent = await spawn(alice, 'weather-monitor');
    try {
      await waitForListedStatus(bob, globexAgent);
      await waitForListedStatus(alice, acmeAgent);
    } finally {
      await killAgent(bob, globexAgent);
      await killAgent(alice, acmeAgent);
    }
  });
});
