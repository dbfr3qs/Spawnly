import { test, expect, type Browser, type APIRequestContext } from '@playwright/test';
import { deleteTemplate, setTemplateStatus } from '../helpers/templates';

// Admin Agent Types UI — the dashboard's management view for agent templates.
// Exercises the real UI (click → fill → save) against the admin-gated routes the
// BFF proxies to the orchestrator → registry:
//   GET    /api/admin/templates                — admin-only full list (incl. disabled)
//   POST   /api/templates                      — create/replace (admin)
//   PATCH  /api/templates/{agentType}          — { status: active|disabled } (admin)
//   DELETE /api/templates/{agentType}          — only when disabled (else 409) (admin)
//
// Plus the non-admin deny path: the viewer user must NOT see the Agent Types nav
// and must get 403 from the admin list + write routes (the server-side gate is
// the real boundary; the hidden nav is cosmetic convenience).
//
// Uses a throwaway agentType so nothing collides with the real example agents.
// afterAll best-effort-tears-it-down (disable then delete, errors ignored).

const TYPE = `admin-ui-${process.pid}`;
const VIEWER_USER = 'viewer';
const VIEWER_PASS = 'viewer';

test.describe('admin agent-types UI', () => {
  test.afterAll(async ({ request }) => {
    // Best-effort cleanup so a mid-test failure can't leak the throwaway type.
    await setTemplateStatus(request, TYPE, 'disabled').catch(() => {});
    await deleteTemplate(request, TYPE).catch(() => {});
  });

  test('admin: create → appears → edit → delete-while-active blocked → disable → delete', async ({ page }) => {
    // 1. Open the admin view (the nav button is shown only for admins).
    await page.goto('/');
    await expect(page.locator('#open-agent-types-btn')).toBeVisible({ timeout: 15_000 });
    await page.locator('#open-agent-types-btn').click();
    // The view replaces the main agent list and fetches the full template set.
    await expect(page.locator('#agent-types-view')).toBeVisible();
    await expect(page.locator('main')).toBeHidden();

    // 2. Create a throwaway type via the guided form.
    await page.locator('#new-agent-type-btn').click();
    await expect(page.locator('#tmpl-form-modal.open')).toBeVisible();
    await page.locator('#tf-agentType').fill(TYPE);
    await page.locator('#tf-version').fill('1');
    await page.locator('#tf-displayName').fill('Admin UI Throwaway');
    await page.locator('#tf-image').fill('ghcr.io/spawnly/noop-agent:latest');

    const [createResp] = await Promise.all([
      page.waitForResponse((r) => r.url().includes('/api/templates') && r.request().method() === 'POST'),
      page.locator('#tmpl-form-save').click(),
    ]);
    // NB: don't read createResp.text() in the assertion message — a successful
    // create is 201 with an empty body, and Playwright's getResponseBody throws
    // ("No data found for resource") on a bodyless response. Status is enough.
    expect(createResp.ok(), `create failed: ${createResp.status()}`).toBeTruthy();
    await expect(page.locator('#tmpl-form-modal.open')).toHaveCount(0);

    // 3. It appears in the admin table (full list, incl. active).
    const row = page.locator(`tr[data-agent-type="${TYPE}"]`);
    await expect(row).toBeVisible({ timeout: 15_000 });
    await expect(row.locator('.badge')).toHaveText(/active/i);

    // 4. Delete-while-active is blocked at the UI: the Delete button is
    //    disabled on an active row (cosmetic; the server still 409s).
    await expect(row.locator('button[data-act="delete"]')).toBeDisabled();

    // 5. Defense-in-depth: a direct API delete of an ACTIVE type is rejected
    //    with 409, independent of the UI's disabled button.
    const guard = await deleteTemplate(page.request, TYPE);
    expect(guard.status(), 'deleting an active template must 409').toBe(409);

    // 6. Edit: change the display name and save (POST replaces the template;
    //    agentType is the immutable key and is read-only in the form).
    await row.locator('button[data-act="edit"]').click();
    await expect(page.locator('#tmpl-form-modal.open')).toBeVisible();
    await expect(page.locator('#tf-agentType')).toHaveAttribute('readonly');
    await page.locator('#tf-displayName').fill('Admin UI Throwaway (renamed)');
    await Promise.all([
      page.waitForResponse((r) => r.url().includes('/api/templates') && r.request().method() === 'POST'),
      page.locator('#tmpl-form-save').click(),
    ]);
    await expect(page.locator('#tmpl-form-modal.open')).toHaveCount(0);

    // 7. Disable via the UI → status badge flips to disabled, Delete enables.
    await Promise.all([
      page.waitForResponse((r) => r.url().includes(`/api/templates/${TYPE}`) && r.request().method() === 'PATCH'),
      row.locator('button[data-act="disable"]').click(),
    ]);
    await expect(row).toHaveClass(/row-disabled/);
    await expect(row.locator('.badge')).toHaveText(/disabled/i);
    await expect(row.locator('button[data-act="delete"]')).toBeEnabled();
    await expect(row.locator('button[data-act="enable"]')).toBeVisible();

    // 8. Delete via the UI (now allowed because the type is disabled). The
    //    delete action opens a native confirm() dialog; auto-accept it.
    page.on('dialog', (d) => d.accept());
    const [delResp] = await Promise.all([
      page.waitForResponse((r) => r.url().includes(`/api/templates/${TYPE}`) && r.request().method() === 'DELETE'),
      row.locator('button[data-act="delete"]').click(),
    ]);
    expect([204, 200]).toContain(delResp.status());

    // 9. Gone from the admin table.
    await expect(row).toHaveCount(0, { timeout: 15_000 });

    // Restore the main agent list when leaving the admin view.
    await page.locator('#agent-types-back-btn').click();
    await expect(page.locator('main')).toBeVisible();
    await expect(page.locator('#agent-types-view')).toBeHidden();
  });

  test('non-admin viewer: nav hidden + admin routes 403', async ({ browser }: { browser: Browser }) => {
    // The cached storageState belongs to alice (admin). The viewer is a
    // different user, so drive the real OIDC login in a FRESH context that
    // carries no cached session, then assert the deny path. Pass an EMPTY
    // storageState explicitly: browser.newContext() inherits the project's
    // configured storageState (alice's session), which would otherwise land us
    // straight on alice's dashboard instead of the login page.
    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    const page = await ctx.newPage();
    try {
      // Real OIDC login as the seeded non-admin viewer.
      await page.goto('/');
      await expect(page.locator('#Username')).toBeVisible({ timeout: 30_000 });
      await page.locator('#Username').fill(VIEWER_USER);
      await page.locator('#Password').fill(VIEWER_PASS);
      await page.locator('button[type="submit"]').click();
      await expect(page.locator('#agent-list')).toBeVisible({ timeout: 30_000 });

      // /api/me must report the viewer as a non-admin — this is the signal that
      // drives the nav gating, so assert it directly (the button ships `hidden`
      // by default, so toBeHidden() alone would pass even if gating regressed).
      const me = await page.request.get('/api/me');
      expect(me.ok(), '/api/me should succeed for a logged-in viewer').toBeTruthy();
      expect((await me.json()).isAdmin, 'viewer must not be reported as admin').toBe(false);

      // The admin nav entry is cosmetic convenience and must be hidden for a
      // non-admin (the server-side gate is the real boundary — see below).
      await expect(page.locator('#open-agent-types-btn')).toBeHidden();

      // Defense-in-depth: even if a tampered client unhid the nav and called
      // the admin routes directly, the BFF/orchestrator gates reject with 403.
      const req: APIRequestContext = page.request;
      const list = await req.get('/api/admin/templates');
      expect(list.status(), 'non-admin must not read the full template list').toBe(403);

      const create = await req.post('/api/templates', { data: { agentType: TYPE, version: '1' } });
      expect(create.status(), 'non-admin must not create templates').toBe(403);

      const patch = await req.patch(`/api/templates/${TYPE}`, { data: { status: 'disabled' } });
      expect(patch.status(), 'non-admin must not change template status').toBe(403);

      const del = await req.delete(`/api/templates/${TYPE}`);
      expect(del.status(), 'non-admin must not delete templates').toBe(403);
    } finally {
      await ctx.close();
    }
  });
});
