import { expect, type Browser, type Page } from '@playwright/test';

// Where the authenticated session is cached so the rest of the suite reuses it
// (see auth.setup.ts and the `setup` project dependency in playwright.config.ts).
export const AUTH_FILE = 'playwright/.auth/user.json';

// The default demo user the shared storage state / bare-{page} suites run as
// (deploy/keycloak/, provisioned by scripts/bootstrap.sh as alice/alice). alice
// is acme's ORG admin — she is NOT the realm/platform admin (that is `root`); the
// setup logs her in scoped to acme (see auth.setup.ts), since as a member of
// multiple orgs an org-less login would land her on the org picker, not the shell.
export const DEMO_USER = 'alice';
export const DEMO_PASS = 'alice';

// Seeded users for the multi-user / multi-org matrix (deploy/keycloak/users.tf +
// scripts/reseed-orgs.sh). password == username for every demo user.
//   root   — PLATFORM master admin: holds the realm `admin` role, member of NO
//            org (the cross-org/Global persona). The ONLY realm admin.
//   alice  — non-realm-admin; ORG admin of acme (SpiceDB tuple, not a realm role)
//   viewer — non-admin; member of acme
//   bob    — non-admin; org admin of globex (cross-org isolation)
//   carol  — non-admin; member of BOTH acme and globex (silent cross-subdomain
//            SSO fixture — org-subdomains e2e case b only; seeded last so she
//            never perturbs the single-org isolation matrix)
//   nomad  — non-admin; member of NO org (drives the "no organization" screen)
export const USERS: Record<string, { pass: string }> = {
  root: { pass: 'root' },
  alice: { pass: 'alice' },
  viewer: { pass: 'viewer' },
  bob: { pass: 'bob' },
  carol: { pass: 'carol' },
  nomad: { pass: 'nomad' },
};

// Per-user (optionally per-org) storage-state path, e.g.
// playwright/.auth/viewer-acme.json — one file per identity the matrix uses.
export function authFile(key: string): string {
  return `playwright/.auth/${key}.json`;
}

// Keycloak's login form field selectors. The default login theme renders the
// username/password inputs as #username/#password and the submit button as
// #kc-login. Centralised here so the spec files assert "the login form is
// showing" without hardcoding Keycloak-specific ids (the previous
// IdentityServer form used #Username/#Password/button[type=submit]).
export const LOGIN_USERNAME = '#username';
export const LOGIN_PASSWORD = '#password';
export const LOGIN_SUBMIT = '#kc-login';

// expectLoginForm asserts the browser is on the (proxied) Keycloak login page —
// i.e. the username field is visible. Used by the unauthenticated specs to
// confirm a logged-out / rejected state.
export async function expectLoginForm(page: Page): Promise<void> {
  await expect(page.locator(LOGIN_USERNAME)).toBeVisible({ timeout: 30_000 });
}

// reachDashboardShell waits for the dashboard shell after the OIDC redirect chain
// settles. The settled shell is EITHER the scoped agent list (#agent-list, an org
// or entered-tenant view) OR the master-admin Global org directory
// ([data-testid="org-directory"], the org-less realm admin's platform landing —
// tenant-visibility Phase 3). A realm admin who belongs to NO org (i.e. `root`)
// lands directly on the directory when orgs exist; on a fresh deploy that seeded
// NONE, they instead hit the "no organization" gate whose "Continue to Global"
// escape leads to the same directory — click it when present. For every org-scoped
// user the escape never appears (ships hidden) and the directory never renders, so
// they simply settle on #agent-list.
//
// NOTE: the Global escape is a per-page-load client action — it sets no cookie, so
// a fresh navigation to '/' re-shows the gate. login()/loginAt() call this on their
// landing; a test that navigates root to '/' again must call it too.
export async function reachDashboardShell(page: Page): Promise<void> {
  const gate = page.locator('#org-gate');
  const escape = page.locator('[data-testid="noorg-continue-global"]');
  const indicator = page.locator('[data-testid="active-org"]');
  const list = page.locator('#agent-list');
  const directory = page.locator('[data-testid="org-directory"]');
  // Poll until the org UX has SETTLED into a terminal state: the active-org
  // indicator is shown (an org, an entered tenant, or the Global landing), the org
  // directory rendered, or the org-less-admin gate surfaced its Global escape.
  await expect(async () => {
    const settled =
      (await indicator.isVisible()) || (await directory.isVisible()) || (await escape.isVisible());
    expect(settled, 'org UX did not settle (no indicator, directory, or Global escape)').toBe(true);
  }).toPass({ timeout: 30_000 });
  if (await escape.isVisible()) {
    await escape.click();
    // Wait for the overlay to fully dismiss so it can't intercept later clicks.
    await expect(gate).toBeHidden({ timeout: 30_000 });
  }
  // Accept EITHER shell. (#agent-list is hidden in the Global directory view, so
  // poll visibility of either rather than a strict single-element assertion.)
  await expect(async () => {
    const shellUp = (await list.isVisible()) || (await directory.isVisible());
    expect(shellUp, 'neither the agent list nor the org directory is visible').toBe(true);
  }).toPass({ timeout: 30_000 });
}

// Drive the real OIDC login UI: navigating to the dashboard while logged out
// redirects to the (proxied) Keycloak login page; fill it and submit. The
// redirect chain lands back on the dashboard shell. Asserts success by waiting
// for #agent-list, which only renders on the dashboard.
//
// Handles Keycloak's IDENTITY-FIRST (username-first) login: with realm
// Organizations enabled, the browser flow presents username on the first screen
// and password on a second (so it can detect an org's brokered IdP before
// asking for a password). The password field is therefore not guaranteed to be
// present alongside the username field — we submit the username and wait for the
// password screen. If the deployment uses the classic single-screen form
// (password already present), the same code path still works.
export async function login(
  page: Page,
  user: string = DEMO_USER,
  pass: string = DEMO_PASS,
  org?: string,
): Promise<void> {
  // When an org is named, start the flow at /signin?org=<alias> so the FIRST token
  // already carries that single-org scope — no intermediate org-less landing +
  // client redirect to race against. Without an org, land on '/' (org-less): a
  // realm admin gets the Global view; a single-org non-admin is auto-selected into
  // their org by the client, both ending on the dashboard shell (#agent-list).
  await page.goto(org ? `/signin?org=${encodeURIComponent(org)}` : '/');
  // Two valid outcomes: the Keycloak login form (no SSO session — the normal
  // fresh-context path), or a SILENT completion straight to the dashboard shell
  // (the context already holds a live Keycloak SSO session; since the dashboard
  // stopped requesting offline_access the online session survives login, so a
  // second login() on the same context never re-prompts). In the silent case the
  // SSO session dictates the user — assert it's the one the caller asked for,
  // so a wrong-user silent login fails loudly instead of polluting the test.
  const uname = page.locator(LOGIN_USERNAME);
  // The active-org indicator is shown on EVERY settled dashboard landing (an org,
  // an entered tenant, or the master-admin Global directory) and never on the
  // Keycloak form, so it — not #agent-list, which is hidden in the Global view — is
  // the reliable "logged in silently" signal across personas.
  const indicator = page.locator('[data-testid="active-org"]');
  await expect(uname.or(indicator).first()).toBeVisible({ timeout: 30_000 });
  if (await indicator.isVisible()) {
    await expect(
      page.locator('#current-user'),
      `silent SSO login landed as the wrong user (wanted ${user}) — use a fresh context to log in as someone else`,
    ).toHaveText(user, { timeout: 10_000 });
    // Settle the shell (org list or Global directory; click the Global escape if a
    // no-orgs deploy surfaced it) before returning so it is actually usable.
    await reachDashboardShell(page);
    return;
  }
  await page.locator(LOGIN_USERNAME).fill(user);

  const password = page.locator(LOGIN_PASSWORD);
  if (!(await password.isVisible())) {
    // Identity-first: submit the username to advance to the password screen.
    await page.locator(LOGIN_SUBMIT).click();
    await expect(password).toBeVisible({ timeout: 30_000 });
  }
  await password.fill(pass);
  await page.locator(LOGIN_SUBMIT).click();
  await reachDashboardShell(page);
}

// ── org-subdomains (satellite) helpers ──────────────────────────────────────
// The apex the local TLS mimic serves (scripts/local-tls.sh flips the cluster
// into APEX_DOMAIN=localtest.me mode). The subdomains suite runs against HTTPS
// under this apex; the default suite (localhost:8090) never touches these.
export const APEX_HOST = 'localtest.me';
export const APEX_ORIGIN = `https://${APEX_HOST}`;

// orgOrigin(slug) → the org's satellite origin, e.g. https://acme.localtest.me.
export function orgOrigin(slug: string): string {
  return `https://${slug}.${APEX_HOST}`;
}

// loginAt drives the OIDC login STARTING at a satellite origin (or the apex),
// tolerating the cross-origin hop to the apex-hosted Keycloak login form and the
// redirect back to the satellite's own /callback. It mirrors login()'s three
// outcomes — the identity-first (username-first) form, a SILENT SSO completion
// (the context already holds a live apex SSO session, so a satellite visit lands
// straight on the scoped shell), and the wrong-user guard — but never assumes the
// baseURL, so it works when `origin` differs from the project baseURL.
//
// `origin` is the FULL origin to start at (use orgOrigin(slug) or APEX_ORIGIN).
// On success the browser is on `origin` showing #agent-list scoped to that org.
export async function loginAt(
  page: Page,
  origin: string,
  user: string = DEMO_USER,
  pass: string = DEMO_PASS,
): Promise<void> {
  await page.goto(`${origin}/`);
  // Either the (apex-origin) Keycloak login form, or a silent landing on the
  // satellite shell (a live apex SSO cookie completes the flow with no prompt).
  const uname = page.locator(LOGIN_USERNAME);
  // The active-org indicator marks a settled dashboard landing (org, entered
  // tenant, or master-admin Global directory) and never the Keycloak form.
  const indicator = page.locator('[data-testid="active-org"]');
  await expect(uname.or(indicator).first()).toBeVisible({ timeout: 30_000 });
  if (await indicator.isVisible()) {
    await expect(
      page.locator('#current-user'),
      `silent SSO login landed as the wrong user (wanted ${user}) — use a fresh context to log in as someone else`,
    ).toHaveText(user, { timeout: 10_000 });
    // Settle the shell (org list or Global directory) before returning.
    await reachDashboardShell(page);
    return;
  }
  await uname.fill(user);
  const password = page.locator(LOGIN_PASSWORD);
  if (!(await password.isVisible())) {
    // Identity-first: submit the username to advance to the password screen.
    await page.locator(LOGIN_SUBMIT).click();
    await expect(password).toBeVisible({ timeout: 30_000 });
  }
  await password.fill(pass);
  await page.locator(LOGIN_SUBMIT).click();
  // The redirect chain lands back on the satellite /callback → the scoped shell.
  // (Or, for an org-less realm admin at the apex, the Global escape — handled here.)
  await reachDashboardShell(page);
}

// contextFor logs a user in (into org, if given) in a FRESH browser context and
// persists the session to a per-user storage-state file, returning the context.
// Fresh contexts don't share Keycloak SSO cookies, so each does a full
// username+password login — the isolation the multi-user matrix needs. The caller
// owns closing the context.
export async function contextFor(browser: Browser, user: string, org?: string) {
  const pass = USERS[user]?.pass ?? user;
  // Empty storageState explicitly: browser.newContext() inherits the project's
  // storageState (alice's session from auth.setup.ts) otherwise — and now that
  // the Keycloak SSO session survives login (offline_access dropped), an
  // inherited alice session would complete /signin SILENTLY as alice instead of
  // prompting for this user. (Before the fix the dead SSO session forced a
  // re-prompt, which masked the inheritance.)
  const ctx = await browser.newContext({ storageState: { cookies: [], origins: [] } });
  const page = await ctx.newPage();
  await login(page, user, pass, org);
  await ctx.storageState({ path: authFile(org ? `${user}-${org}` : user) });
  return ctx;
}
