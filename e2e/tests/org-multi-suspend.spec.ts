import { test, expect } from '@playwright/test';
import { contextFor } from '../helpers/auth';

// Multi-org suspension (account-lifecycle follow-up, items A/1-2): a user who
// belongs to TWO orgs disabled by BOTH is disabled by the SET of both, and stays
// locked out until EVERY org lifts its suspension — proving disabledByOrg is a
// multi-valued set live, not a single value one org-enable can wrongly clear.
//
// Root-API-driven against THROWAWAY orgs + a throwaway shared member (no UI login,
// so it's cheap and can't disturb the seeded acme/globex a shared `make e2e` run
// relies on). Names are unique per run for idempotency across repeated runs.

function uniqueSuffix(): string {
  return `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}`
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');
}

test.describe('multi-org suspension', () => {
  test.describe.configure({ timeout: 300_000 });

  test('a member of two disabled orgs stays disabled until BOTH enable', async ({ browser }) => {
    const suffix = uniqueSuffix();
    const org1 = `msa-${suffix}`;
    const org2 = `msb-${suffix}`;
    const admin1 = `msadmin1-${suffix}`;
    const admin2 = `msadmin2-${suffix}`;
    const member = `msmember-${suffix}`;
    const enc = encodeURIComponent;

    const rootCtx = await contextFor(browser, 'root');
    const root = await rootCtx.newPage();
    await root.goto('/');

    const userEnabled = async (username: string): Promise<boolean | undefined> => {
      const resp = await root.request.get('/api/admin/users');
      if (!resp.ok()) return undefined;
      const users: Array<{ username?: string; enabled?: boolean }> = await resp.json();
      return users.find((u) => u.username === username)?.enabled;
    };

    try {
      // Two throwaway orgs (platform-admin onboarding path pins a readable slug).
      for (const [org, admin] of [[org1, admin1], [org2, admin2]] as const) {
        expect(
          (await root.request.post('/api/admin/orgs', { data: { orgId: org, adminUser: admin } })).status(),
          `create ${org}`,
        ).toBe(201);
        // A single member who belongs to BOTH orgs.
        expect(
          (await root.request.post(`/api/admin/orgs/${enc(org)}/members`, {
            data: { user: member, role: 'member' },
          })).status(),
          `add shared member to ${org}`,
        ).toBe(201);
      }
      expect(await userEnabled(member), 'member starts enabled').toBe(true);

      // Both orgs suspend → the shared member is disabled by BOTH.
      expect((await root.request.post(`/api/admin/orgs/${enc(org1)}/disable`)).status(), 'disable org1').toBe(200);
      expect((await root.request.post(`/api/admin/orgs/${enc(org2)}/disable`)).status(), 'disable org2').toBe(200);
      expect(await userEnabled(member), 'member disabled after both suspend').toBe(false);

      // Org1 lifts — org2 STILL suspends the member, so they stay disabled. This is
      // the invariant a single-valued attribute would break (org1-enable would clear
      // the lone value and wrongly re-enable).
      expect((await root.request.post(`/api/admin/orgs/${enc(org1)}/enable`)).status(), 'enable org1').toBe(200);
      expect(await userEnabled(member), 'member STAYS disabled while org2 still suspends').toBe(false);

      // Org2 lifts — the set is now empty, so the member is finally re-enabled.
      expect((await root.request.post(`/api/admin/orgs/${enc(org2)}/enable`)).status(), 'enable org2').toBe(200);
      expect(await userEnabled(member), 'member re-enabled once BOTH orgs lifted').toBe(true);
    } finally {
      // Best-effort cleanup: ensure both orgs are enabled, then delete orgs + users.
      for (const org of [org1, org2]) {
        await root.request.post(`/api/admin/orgs/${enc(org)}/enable`).catch(() => {});
        await root.request.delete(`/api/admin/orgs/${enc(org)}`).catch(() => {});
      }
      for (const u of [member, admin1, admin2]) {
        await root.request.delete(`/api/admin/users/${enc(u)}`).catch(() => {});
      }
      await rootCtx.close().catch(() => {});
    }
  });
});
