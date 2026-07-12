import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { contextFor, LOGIN_USERNAME } from '../helpers/auth';

// Global caps (signup-lifecycle Phase 2, spec criteria 3 & 4). Two env-configurable
// ceilings — MAX_USERS (a Keycloak registration-flow SPI) and MAX_ORGS (the
// registry's self-serve org path) — reject the (N+1)th SELF-SERVE creation while a
// master admin still provisions beyond the cap.
//
// DESTRUCTIVE + SERIAL — read before touching:
//   * Toggling a cap means `kubectl set env` on a Deployment, which RESTARTS its
//     pod. On kind the Keycloak dev-file DB is wiped on restart (no PVC), so after
//     a keycloak env change the realm MUST be re-applied (scripts/apply-realm.sh,
//     which also reseeds templates + demo orgs). Re-applying the realm rotates the
//     realm's signing keys, so it also INVALIDATES every cached session — hence the
//     afterAll refreshes alice's shared storage state so later specs re-auth
//     cleanly. The registry's in-memory store is likewise wiped on its restart.
//   * This is why the suite runs single-worker/serial (playwright.config.ts). This
//     describe is additionally test.describe.serial and every test restores the
//     cluster (unset the cap + roll + re-apply/reseed) in a finally/afterAll so a
//     later suite always sees an UNCAPPED cluster, even on failure.
//   * These tests are heavy (pod restarts + a terraform realm apply each). The
//     per-test timeout is raised well above the config default.

const REPO = `${process.cwd()}/..`; // playwright runs from e2e/

function kubectl(args: string[], opts: { input?: string } = {}): string {
  return execFileSync('kubectl', args, { input: opts.input, stdio: ['pipe', 'pipe', 'inherit'] })
    .toString();
}

// setEnv patches an env var on a Deployment and waits for the new pod to roll out.
// Pass `NAME=value` to set, or `NAME-` (trailing dash) to unset — kubectl's own
// syntax. The rollout wait makes the change observable before the test proceeds.
function setEnv(deploy: string, envArg: string) {
  kubectl(['set', 'env', `deployment/${deploy}`, envArg]);
  kubectl(['rollout', 'status', `deployment/${deploy}`, '--timeout=300s']);
}

// applyRealm re-applies the spawnly realm (recreating it after the keycloak
// restart wiped the dev-file DB) and reseeds templates + demo orgs. Idempotent;
// reconstructs its TF vars from the cluster secrets.
function applyRealm() {
  execFileSync('bash', [`${REPO}/scripts/apply-realm.sh`], {
    stdio: ['ignore', 'inherit', 'inherit'],
    timeout: 540_000,
  });
  // apply-realm.sh rolls the dashboard (+registry/orchestrator), so the webServer's
  // reconnecting :8090 port-forward briefly points at a terminating/starting pod —
  // a navigation in that window gets net::ERR_CONNECTION_REFUSED / ERR_EMPTY_RESPONSE.
  // Block here until :8090 serves again (any HTTP response, incl. the 302 to signin)
  // so EVERY navigation after applyRealm() is safe, rather than hardening each one.
  execFileSync(
    'bash',
    [
      '-c',
      'for i in $(seq 1 60); do curl -s -o /dev/null http://localhost:8090/ && exit 0; sleep 2; done; echo "dashboard :8090 did not recover after apply-realm" >&2; exit 1',
    ],
    { stdio: ['ignore', 'inherit', 'inherit'], timeout: 140_000 },
  );
}

// reseedOrgs recreates the demo orgs (acme, globex) through the registry's ADMIN
// onboarding path (not self-serve, so it is never gated by MAX_ORGS). Used to
// give the registry a known org count after its store was wiped by a restart.
function reseedOrgs() {
  execFileSync('bash', [`${REPO}/scripts/reseed-orgs.sh`], {
    stdio: ['ignore', 'inherit', 'inherit'],
    timeout: 180_000,
  });
}

// seedTemplates reseeds the agent templates into the registry. Toggling MAX_ORGS
// restarts the registry, which wipes its in-memory store — templates AND orgs. The
// MAX_ORGS test therefore MUST reseed templates too (not just orgs), or every
// later template-dependent spec (chain-worker, ciba-consent, template-lifecycle…)
// runs against a registry with zero templates and fails.
function seedTemplates() {
  execFileSync('bash', [`${REPO}/scripts/seed.sh`], {
    stdio: ['ignore', 'inherit', 'inherit'],
    timeout: 180_000,
  });
}

// refreshAliceSession re-logs alice in and overwrites the shared storage-state
// file the chromium project uses, so specs that run AFTER this one (and rely on
// the cached session) are not stranded by the realm-key rotation above.
async function refreshAliceSession(browser: import('@playwright/test').Browser) {
  // Re-applying the realm above rotates the realm signing keys, invalidating every
  // cached session — so re-establish the shared storage state that specs running
  // AFTER this suite rely on. contextFor performs the robust identity-first login
  // (the same helper the rest of the matrix and the teardown use) and persists
  // playwright/.auth/alice-acme.json; the chromium project default is
  // playwright/.auth/user.json, so save the SAME authenticated state there too.
  // (Reusing contextFor avoids a brittle hand-rolled re-login that mishandled
  // alice's multi-org org-picker landing.)
  // apply-realm.sh rolls the dashboard, so for a few seconds after it returns the
  // reconnecting port-forward can hit a still-starting pod (ERR_EMPTY_RESPONSE) —
  // retry a few times with a short settle so that transient window doesn't fail
  // this cleanup step.
  let lastErr: unknown;
  for (let attempt = 1; attempt <= 4; attempt++) {
    try {
      const ctx = await contextFor(browser, 'alice', 'acme');
      await ctx.storageState({ path: 'playwright/.auth/user.json' });
      await ctx.close();
      return;
    } catch (err) {
      lastErr = err;
      await new Promise((r) => setTimeout(r, 5_000));
    }
  }
  throw lastErr;
}

function uniqueSuffix(): string {
  return `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}`
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');
}

test.describe.serial('global caps', () => {
  test.describe.configure({ timeout: 900_000 });

  test('MAX_USERS rejects the self-serve register form; admin creation bypasses it', async ({
    browser,
  }) => {
    // Arm the user cap, then re-apply the realm so the freshly-restarted keycloak
    // has the spawnly realm (with the registration-flow SPI binding) back. The
    // realm's seeded users (root/alice/bob/carol/viewer + demo) put the count well
    // above 1, so the very next self-serve registration is over the cap.
    setEnv('keycloak', 'MAX_USERS=1');
    applyRealm();

    try {
      const suffix = uniqueSuffix();
      const username = `capuser-${suffix}`;
      const password = `Pw-${suffix}-9`;
      const email = `${username}@example.test`;

      // (a) Self-serve registration is rejected on the register form with the
      //     friendly capacity message — the cap's validate() runs before user
      //     creation, so no account (and no verification email) is ever produced.
      const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
      try {
        const page = await ctx.newPage();
        await page.goto('/');
        await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
        await page.getByRole('link', { name: /register/i }).first().click();

        await expect(page.locator('#username')).toBeVisible({ timeout: 30_000 });
        await page.locator('#firstName').fill('Cap');
        await page.locator('#lastName').fill('User');
        await page.locator('#email').fill(email);
        await page.locator('#username').fill(username);
        await page.locator('#password').fill(password);
        await page.locator('#password-confirm').fill(password);
        await page.locator('#kc-form-buttons input[type="submit"]').click();

        // Still on the register page, now showing the capacity error.
        await expect(page.locator('#username')).toBeVisible({ timeout: 30_000 });
        await expect(page.locator('body')).toContainText(/capacity/i, { timeout: 30_000 });
      } finally {
        await ctx.close();
      }

      // (b) Admin bypass: as root, add a brand-new username to acme via the admin
      //     membership API. That path auto-creates the Keycloak user through the
      //     adapter's createUser (admin API, NOT the registration flow), so it is
      //     never gated by MAX_USERS.
      const rootCtx = await contextFor(browser, 'root');
      try {
        const root = await rootCtx.newPage();
        await root.goto('/');
        const newUser = `capbypass-${suffix}`;
        const resp = await root.request.post('/api/admin/orgs/acme/members', {
          data: { user: newUser, role: 'member' },
        });
        expect(
          resp.ok(),
          `admin member-add (auto-creating a KC user) should bypass MAX_USERS: ${resp.status()} ${await resp
            .text()
            .catch(() => '')}`,
        ).toBeTruthy();
        // Tidy the membership (the KC user itself is dropped when the realm is
        // re-applied fresh in the restore below).
        await root.request
          .delete(`/api/admin/orgs/acme/members/${encodeURIComponent(newUser)}`)
          .catch(() => {});
      } finally {
        await rootCtx.close();
      }
    } finally {
      // Restore an UNCAPPED cluster for later suites and refresh the shared session.
      setEnv('keycloak', 'MAX_USERS-');
      applyRealm();
      await refreshAliceSession(browser);
    }
  });

  test('MAX_ORGS rejects self-serve org creation; admin creation bypasses it', async ({
    browser,
  }) => {
    let createdOrgId = '';
    try {
      // Arm the org cap on the registry (restart wipes its in-memory store — both
      // templates and orgs), then reseed templates + the demo orgs through the
      // ADMIN path so the org count (3) is already at/over the cap of 1. Arming is
      // inside the try so the finally below always restores an uncapped registry
      // even if a reseed step fails mid-arm.
      setEnv('registry', 'MAX_ORGS=1');
      seedTemplates();
      reseedOrgs();

      const rootCtx = await contextFor(browser, 'root');
      try {
        const root = await rootCtx.newPage();
        await root.goto('/');

        // (a) Self-serve org creation is rejected with the friendly 403 JSON. Driven
        //     through the same session-gated /api/signup/org the no-org screen posts
        //     to (?selfServe=1 at the registry), which is the capped path.
        const rejected = await root.request.post('/api/signup/org', {
          data: { name: `Capped Org ${uniqueSuffix()}` },
        });
        expect(rejected.status(), `self-serve org over the cap should be 403`).toBe(403);
        const body = await rejected.json().catch(() => ({}) as any);
        expect(String(body.error ?? ''), 'a friendly capacity message').toMatch(/capacity/i);

        // (b) Admin bypass: the platform-admin onboarding path creates an org past
        //     the cap. adminUser=root keeps root org-less-elsewhere semantics simple.
        const orgName = `Admin Bypass ${uniqueSuffix()}`;
        const created = await root.request.post('/api/admin/orgs', {
          data: { name: orgName, adminUser: 'root' },
        });
        expect(
          created.ok(),
          `admin org create should bypass MAX_ORGS: ${created.status()} ${await created
            .text()
            .catch(() => '')}`,
        ).toBeTruthy();
        createdOrgId = (await created.json().catch(() => ({}) as any)).orgId ?? '';

        // Clean up the admin-created org so it doesn't leak into later suites.
        if (createdOrgId) {
          await root.request
            .delete(`/api/admin/orgs/${encodeURIComponent(createdOrgId)}`)
            .catch(() => {});
        }
      } finally {
        await rootCtx.close();
      }
    } finally {
      // Restore an UNCAPPED registry and reseed templates + demo orgs for later
      // suites (the restart above wiped the store).
      setEnv('registry', 'MAX_ORGS-');
      seedTemplates();
      reseedOrgs();
    }
  });
});
