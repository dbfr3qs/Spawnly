import { test, expect, type Page } from '@playwright/test';
import { LOGIN_USERNAME, LOGIN_PASSWORD, LOGIN_SUBMIT } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';
import { killAgent } from '../helpers/cleanup';
import { verificationLink, clearMessages } from '../helpers/mailpit';

// Self-serve signup (multi-tenancy Phase 7, spec criterion 9). A brand-new user
// registers through the Keycloak registration form (realm registration_allowed,
// spawnly theme), lands on the dashboard's no-organization screen, creates an
// org there (becoming its admin), and spawns from the global catalog.
//
// This runs in a FRESH, logged-out browser context (the chromium project
// otherwise reuses alice's cached session). Users/orgs persist in Keycloak until
// its next restart, so the username + org slug are made unique per run to keep
// the spec idempotent across repeated `make e2e` runs on the same cluster.

// A short, DNS-label-safe, unique id (lowercase alnum only). Generated per test
// ATTEMPT (not per module load) so a Playwright retry gets a fresh user + org
// slug rather than colliding with the previous attempt's (409 / "user exists").
function uniqueSuffix(): string {
  return `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}`
    .toLowerCase()
    .replace(/[^a-z0-9]/g, '');
}

// finishReauth completes the post-signup /switch-org re-auth. Under realm
// Organizations this is NOT password-less (Phase 5 finding), so Keycloak's
// identity-first form usually re-appears — but handle the SSO-silent case too by
// waiting for whichever of the dashboard shell or the login form shows first.
async function finishReauth(page: Page, user: string, pass: string) {
  await expect(async () => {
    const onDash = await page.locator('#agent-list').isVisible().catch(() => false);
    const onForm = await page.locator(LOGIN_USERNAME).isVisible().catch(() => false);
    expect(onDash || onForm).toBeTruthy();
  }).toPass({ timeout: 30_000 });

  if (await page.locator(LOGIN_USERNAME).isVisible().catch(() => false)) {
    await page.locator(LOGIN_USERNAME).fill(user);
    const pw = page.locator(LOGIN_PASSWORD);
    if (!(await pw.isVisible())) {
      await page.locator(LOGIN_SUBMIT).click();
      await expect(pw).toBeVisible({ timeout: 30_000 });
    }
    await pw.fill(pass);
    await page.locator(LOGIN_SUBMIT).click();
  }
  await expect(page.locator('#agent-list')).toBeVisible({ timeout: 60_000 });
}

// verifyEmail completes the Phase-1 email-verification step: poll Mailpit for the
// verification mail sent to `email`, visit the Keycloak action-token link, and
// settle on the dashboard. Keycloak either resumes the still-open registration
// auth session (landing straight on the dashboard) or drops the user back to the
// login form — handle both by logging in with the just-set credentials when the
// form reappears. Returns once the browser is back on the dashboard origin.
async function verifyEmail(page: Page, email: string, user: string, pass: string) {
  const link = await verificationLink(email);
  await page.goto(link);

  // After verifying, Keycloak lands on EITHER the dashboard (resumed session →
  // no-org screen or shell) or its login form (fresh action-token context).
  const noOrg = page.locator('[data-testid="no-org-screen"]');
  const uname = page.locator(LOGIN_USERNAME);
  await expect(noOrg.or(uname).first()).toBeVisible({ timeout: 60_000 });
  if (await uname.isVisible().catch(() => false)) {
    await uname.fill(user);
    const pw = page.locator(LOGIN_PASSWORD);
    if (!(await pw.isVisible())) {
      await page.locator(LOGIN_SUBMIT).click(); // identity-first: advance to password
      await expect(pw).toBeVisible({ timeout: 30_000 });
    }
    await pw.fill(pass);
    await page.locator(LOGIN_SUBMIT).click();
  }
}

test.describe('self-serve signup', () => {
  test('a new user registers, creates an org, and spawns from the global catalog', async ({
    browser,
  }) => {
    const suffix = uniqueSuffix();
    const username = `signup-${suffix}`;
    const password = `Pw-${suffix}-9`;
    const email = `${username}@example.test`;
    const orgName = `Org ${suffix}`;

    // Start from an empty sink so the poll can't pick up a stale verification mail
    // from an earlier run (recipients are per-run unique anyway, but keep it tidy).
    await clearMessages().catch(() => {});

    const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
    let spawnedId = '';
    let page: Page;
    try {
      page = await ctx.newPage();

      // 1) Logged out → Keycloak login page. Follow its "Register" link so the
      //    OIDC flow (client_id/redirect_uri/state) is preserved through signup.
      await page.goto('/');
      await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
      const registerLink = page.getByRole('link', { name: /register/i }).first();
      await expect(registerLink, 'the login page must offer a Register link').toBeVisible({
        timeout: 30_000,
      });
      await registerLink.click();

      // 2) Fill Keycloak's registration form (default user profile: first/last/
      //    email/username/password). Field ids are keycloak.v2's register.ftl.
      await expect(page.locator('#username')).toBeVisible({ timeout: 30_000 });
      await page.locator('#firstName').fill('New');
      await page.locator('#lastName').fill('User');
      await page.locator('#email').fill(email);
      await page.locator('#username').fill(username);
      await page.locator('#password').fill(password);
      await page.locator('#password-confirm').fill(password);
      await page.locator('#kc-form-buttons input[type="submit"]').click();

      // 3) Email verification (signup-lifecycle Phase 1). With SMTP configured the
      //    realm now requires a verified email: instead of logging straight in, the
      //    submit lands on Keycloak's "verify your email" interstitial and a
      //    verification mail is sent to Mailpit. Poll for it, then visit the
      //    action-token link — proving the SMTP path delivered (criterion 1).
      await verifyEmail(page, email, username, password);

      // 3b) Verified → logged in → redirected back to the dashboard. With no org
      //    (and no org scope requested), the user lands on the no-organization
      //    screen — which now hosts the self-serve create-org form.
      await expect(page.locator('[data-testid="no-org-screen"]')).toBeVisible({ timeout: 60_000 });
      await expect(page.locator('[data-testid="create-org-form"]')).toBeVisible();
      // No org yet, so no agents.
      expect(await listAgents(page)).toEqual([]);

      // 4) Create the org — the user supplies ONLY a display name; the registry
      //    mints the org's opaque GUID id. On success the form reloads, /api/me
      //    reports the user's single new org, and the dashboard auto-selects it via
      //    a /switch-org re-auth. Sync on the signup response so the re-auth handling
      //    can't race the async redirect the form issues.
      await page.locator('[data-testid="create-org-name"]').fill(orgName);
      const [signupResp] = await Promise.all([
        page.waitForResponse(
          (r) => r.url().includes('/api/signup/org') && r.request().method() === 'POST',
        ),
        page.locator('[data-testid="create-org-submit"]').click(),
      ]);
      // On 201 the page navigates immediately (the response body is then evicted),
      // so only read the body on the error path — where the form shows an error and
      // does NOT navigate.
      const status = signupResp.status();
      const detail = status === 201 ? '' : await signupResp.text().catch(() => '');
      expect(status, `signup failed: ${status} ${detail}`).toBe(201);

      // Complete the /switch-org re-auth, landing on the dashboard in the new org.
      await finishReauth(page, username, password);

      // 5) Arrived as the org admin: the active org is shown, and org-admin
      //    surfaces (Agent Types + Members) are offered (isOrgAdmin from /api/me).
      await expect(page.locator('[data-testid="active-org"]')).toContainText(
        new RegExp(orgName.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'), 'i'),
        { timeout: 30_000 },
      );
      await expect(page.locator('#open-members-btn')).toBeVisible({ timeout: 30_000 });

      // 6) Spawn from the GLOBAL catalog (weather-monitor is seeded global).
      spawnedId = await spawn(page, 'weather-monitor');
      await expect
        .poll(async () => (await listAgents(page)).some((a) => a.agentId === spawnedId), {
          timeout: 60_000,
        })
        .toBe(true);
    } finally {
      if (spawnedId && page!) await killAgent(page!, spawnedId).catch(() => {});
      await ctx.close();
    }
  });
});
