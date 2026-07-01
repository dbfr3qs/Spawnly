import { test, expect, type Page } from '@playwright/test';
import {
  cloneSampleTemplate,
  deleteTemplate,
  registerTemplate,
  setTemplateStatus,
  type Template,
} from '../helpers/templates';

// Scenario — the spawn modal's Tenant field is conditional on the selected agent
// type's requiresTenant flag (sourced from the non-admin GET /api/templates/spawn
// list). For a requiresTenant:true type the field shows and is required; for any
// other type it is hidden and the agent spawns GLOBAL (no tenantId in the POST).
//
// Two throwaway types are registered so nothing collides with the real example
// agents on the shared cluster; afterAll tears them down (disable → delete).
//
// /api/spawn is stubbed (page.route) so we assert the exact payload the client
// builds — field visibility and payload construction are purely client-side, and
// the orchestrator's server-side requiresTenant enforcement is covered by the Go
// unit tests. Stubbing also keeps the test hermetic (no real agents spun up).
const TENANTED = 'disposable-tenanted';
const GLOBAL = 'disposable-global';

function tenantedTemplate(): Template {
  const t = cloneSampleTemplate(TENANTED);
  t.requiresTenant = true;
  return t;
}

function globalTemplate(): Template {
  const t = cloneSampleTemplate(GLOBAL);
  t.requiresTenant = false;
  return t;
}

// Open the spawn modal and pick an agent type; the page populates #f-type from
// GET /api/templates/spawn asynchronously, so wait for the option to attach.
async function openAndSelect(page: Page, agentType: string) {
  const select = page.locator('#f-type');
  await expect(select.locator(`option[value="${agentType}"]`)).toHaveCount(1, { timeout: 15_000 });
  await select.selectOption(agentType);
}

test.describe('spawn-tenant-field', () => {
  test.afterAll(async ({ request }) => {
    for (const type of [TENANTED, GLOBAL]) {
      await setTemplateStatus(request, type, 'disabled').catch(() => {});
      await deleteTemplate(request, type).catch(() => {});
    }
  });

  test('Tenant field shows only for requiresTenant types; global spawn omits tenantId', async ({
    page,
    request,
  }) => {
    await registerTemplate(request, tenantedTemplate());
    await registerTemplate(request, globalTemplate());

    // Capture every /api/spawn payload and stub a success so no real agent is
    // created. postDataJSON() is the exact body the client built.
    const spawnPayloads: Array<Record<string, unknown>> = [];
    await page.route('**/api/spawn', async (route) => {
      spawnPayloads.push(route.request().postDataJSON());
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({ workloadName: 'stub-agent' }),
      });
    });

    await page.goto('/');
    await page.locator('#open-modal-btn').click();

    const tenantGroup = page.locator('#f-tenant-group');

    // requiresTenant:true → field visible.
    await openAndSelect(page, TENANTED);
    await expect(tenantGroup).toBeVisible();

    // requiresTenant:false → field hidden (live toggle on change).
    await openAndSelect(page, GLOBAL);
    await expect(tenantGroup).toBeHidden();

    // Switch back → visible again (toggle is reversible, not one-way).
    await openAndSelect(page, TENANTED);
    await expect(tenantGroup).toBeVisible();

    // Blank tenant on a requiresTenant type blocks the spawn: no POST is issued
    // and a validation toast is shown.
    await page.locator('#f-tenant').fill('');
    const before = spawnPayloads.length;
    await page.locator('#spawn-btn').click();
    await expect(page.locator('#toast.show')).toContainText(/tenant id is required/i);
    expect(spawnPayloads.length, 'blank required tenant must not POST').toBe(before);

    // Valid tenant → POST includes tenantId.
    await page.locator('#f-tenant').fill('tenant-42');
    await page.locator('#spawn-btn').click();
    await expect.poll(() => spawnPayloads.length).toBe(before + 1);
    const tenantedBody = spawnPayloads[spawnPayloads.length - 1];
    expect(tenantedBody.agentType).toBe(TENANTED);
    expect(tenantedBody.tenantId).toBe('tenant-42');

    // Global type → field hidden, POST omits tenantId entirely (spawns global).
    // The hidden input still holds the prior tenant-42 value, so a passing
    // assertion proves the omission comes from the payload guard (not merely an
    // empty field) — i.e. a stale tenant cannot leak into a global spawn.
    await page.locator('#open-modal-btn').click();
    await openAndSelect(page, GLOBAL);
    await expect(tenantGroup).toBeHidden();
    await expect(page.locator('#f-tenant')).toHaveValue('tenant-42');
    const beforeGlobal = spawnPayloads.length;
    await page.locator('#spawn-btn').click();
    await expect.poll(() => spawnPayloads.length).toBe(beforeGlobal + 1);
    const globalBody = spawnPayloads[spawnPayloads.length - 1];
    expect(globalBody.agentType).toBe(GLOBAL);
    expect('tenantId' in globalBody, 'global spawn must omit tenantId').toBe(false);
  });
});
