import { test, expect } from '@playwright/test';
import {
  cloneSampleTemplate,
  deleteTemplate,
  isTypeOffered,
  registerTemplate,
  setTemplateStatus,
} from '../helpers/templates';

// Scenario — template lifecycle. Exercises the control-plane API the dashboard
// proxies to the orchestrator (register → list/offer → delete-guard → disable →
// hide → unspawnable → delete → gone) end-to-end against the real registry.
//
// Uses a throwaway type so nothing it registers can collide with the real
// example agents on the shared cluster. afterAll best-effort-tears-it-down
// (disable then delete, errors ignored) so a mid-test failure can't leak the
// type into the cluster and pollute later runs.
const TYPE = 'disposable-worker';

test.describe('template-lifecycle', () => {
  test.afterAll(async ({ request }) => {
    // Must be disabled before it can be deleted (active → 409). Both calls are
    // best-effort: the happy path already deleted it, so these typically no-op.
    await setTemplateStatus(request, TYPE, 'disabled').catch(() => {});
    await deleteTemplate(request, TYPE).catch(() => {});
  });

  test('register → disable → hide → unspawnable → delete', async ({ page, request }) => {
    // 1. Register a fresh, schema-valid clone of a real template.
    await registerTemplate(request, cloneSampleTemplate(TYPE));

    // 2. It IS offered — proves it landed in GET /api/templates (active types).
    await page.goto('/');
    expect(await isTypeOffered(page, TYPE), 'newly registered type should be offered').toBe(true);

    // 3. Delete-guard: deleting an ACTIVE template is rejected with 409.
    const guard = await deleteTemplate(request, TYPE);
    expect(guard.status(), 'deleting an active template must 409').toBe(409);

    // 4. Disable it.
    const disabled = await setTemplateStatus(request, TYPE, 'disabled');
    expect(disabled.status(), 'disabling an active template must 200').toBe(200);

    // 5. It is now HIDDEN — GET /api/templates excludes disabled types, so the
    //    spawn dropdown no longer offers it. Reload so the page refetches.
    await page.reload();
    expect(await isTypeOffered(page, TYPE), 'a disabled type must not be offered').toBe(false);

    // 6. Defense-in-depth: spawning a disabled type is rejected by the
    //    orchestrator with 409, independent of the UI hiding it. The dashboard
    //    builds the spawn body as { agentType, tenantId, task? } (see
    //    cmd/dashboard/static/index.html), with userId injected server-side from
    //    the session — so a minimal direct POST mirrors a real spawn attempt.
    const spawn = await request.post('/api/spawn', {
      data: { agentType: TYPE, tenantId: 'tenant-1' },
    });
    expect(spawn.status(), 'spawning a disabled type must 409').toBe(409);

    // 7. Now that it is disabled, delete succeeds with 204.
    const deleted = await deleteTemplate(request, TYPE);
    expect(deleted.status(), 'deleting a disabled template must 204').toBe(204);

    // 8. Proof of deletion: a second delete on the now-absent type is 404.
    const gone = await deleteTemplate(request, TYPE);
    expect(gone.status(), 'deleting an absent template must 404').toBe(404);
  });
});
