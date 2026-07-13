import { test, expect } from '@playwright/test';
import { execFileSync } from 'node:child_process';
import { createHash, randomBytes } from 'node:crypto';

// Brute-force protection (signup-lifecycle Phase 1, spec criterion 2). The realm
// TF turns on `security_defenses { brute_force_detection {} }` with Keycloak's
// stock defaults (temporary per-user lockout after 30 consecutive failures). This
// spec proves BOTH halves of the criterion:
//   (a) config — the realm reports bruteForceProtected: true (read via kcadm);
//   (b) functional — a throwaway verified user, hit with 30 rapid bad-password
//       attempts, is then temporarily rejected on a CORRECT password.
//
// The bad-login attempts drive Keycloak's real browser auth flow over Node fetch
// through the dashboard origin (which proxies /realms/* single-origin), using the
// public `mobile` client — the same technique as helpers/mobile.ts — so 30
// attempts run at request speed rather than as full browser navigations.

const KC = '/opt/keycloak/bin/kcadm.sh';

// kcadm runs a kcadm.sh subcommand inside the keycloak pod, authenticated as the
// bootstrap master admin (from the keycloak-admin Secret). Returns stdout.
function kcadm(args: string[]): string {
  const adminPw = Buffer.from(
    execFileSync('kubectl', ['get', 'secret', 'keycloak-admin', '-o', 'jsonpath={.data.password}']).toString(),
    'base64',
  ).toString();
  // bootstrap.sh generates this as `openssl rand -hex 24`, so single-quoting it
  // into the script below is injection-safe. Assert that invariant rather than
  // rely on it silently — if the generator ever changes to include a quote, this
  // fails loudly instead of becoming a shell-injection point.
  if (!/^[0-9a-f]+$/.test(adminPw)) {
    throw new Error('unexpected keycloak-admin password format (expected hex) — refusing to interpolate into a shell script');
  }
  // config credentials writes ~/.keycloak/kcadm.config in the pod, then the real
  // command runs against it — chained so they share that file. The whole script
  // (which carries the admin password) is fed to `bash -s` over STDIN rather than
  // as an argv, so the password never appears in the pod/host process list. The
  // password is hex (safe to single-quote inside the script).
  const script =
    `${KC} config credentials --server http://localhost:8080 --realm master ` +
    `--user admin --password '${adminPw}' >/dev/null 2>&1 && ` +
    `${KC} ${args.join(' ')}`;
  return execFileSync('kubectl', ['exec', '-i', 'deploy/keycloak', '--', 'bash', '-s'], {
    input: script,
  }).toString();
}

function base64url(b: Buffer): string {
  return b.toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

// browserLoginAttempt drives the identity-first browser login for `user`/`pass`
// end-to-end over fetch (public `mobile` client + PKCE), returning true iff it
// obtained an authorization code — i.e. the credentials were accepted AND the
// account was not locked. A rejected or locked login never reaches the redirect.
async function browserLoginAttempt(origin: string, user: string, pass: string): Promise<boolean> {
  const verifier = base64url(randomBytes(32));
  const challenge = base64url(createHash('sha256').update(verifier).digest());
  const redirectUri = 'spawnly://auth';
  const params = new URLSearchParams({
    client_id: 'mobile',
    response_type: 'code',
    redirect_uri: redirectUri,
    scope: 'openid',
    code_challenge: challenge,
    code_challenge_method: 'S256',
    state: base64url(randomBytes(8)),
  });

  const authPage = await fetch(
    `${origin}/realms/spawnly/protocol/openid-connect/auth?${params.toString()}`,
    { redirect: 'manual' },
  );
  const jar = new Map<string, string>();
  const stash = (resp: Response) => {
    for (const c of resp.headers.getSetCookie?.() ?? []) {
      const pair = c.split(';')[0];
      const eq = pair.indexOf('=');
      if (eq > 0) jar.set(pair.slice(0, eq).trim(), pair.slice(eq + 1).trim());
    }
  };
  const cookieHeader = () => [...jar].map(([k, v]) => `${k}=${v}`).join('; ');
  const formAction = (html: string) =>
    html.match(/id="kc-form-login"[\s\S]*?action="([^"]+)"/)?.[1]?.replace(/&amp;/g, '&');

  let location = authPage.headers.get('location');
  if (location && location.startsWith(redirectUri)) return true; // already SSO'd (not our case)

  stash(authPage);
  let html = await authPage.text();
  for (let step = 0; step < 3; step++) {
    const action = formAction(html);
    if (!action) return false; // no form to submit → treat as a failed attempt
    const form = new URLSearchParams();
    if (/name="username"/.test(html)) form.set('username', user);
    if (/name="password"/.test(html) || /type="password"/.test(html)) form.set('password', pass);

    let resp = await fetch(action, {
      method: 'POST',
      headers: { 'Content-Type': 'application/x-www-form-urlencoded', Cookie: cookieHeader() },
      body: form.toString(),
      redirect: 'manual',
    });
    stash(resp);
    location = resp.headers.get('location');
    if (location && location.startsWith(redirectUri)) return true;
    if (location) {
      resp = await fetch(location, { headers: { Cookie: cookieHeader() }, redirect: 'manual' });
      stash(resp);
      location = resp.headers.get('location');
      if (location && location.startsWith(redirectUri)) return true;
    }
    html = await resp.text();
  }
  return false;
}

test.describe('brute-force protection', () => {
  test('the realm has brute-force protection enabled (config assertion)', async () => {
    const out = kcadm(['get', 'realms/spawnly', '--fields', 'bruteForceProtected']);
    expect(out, `realm did not report bruteForceProtected: true — got: ${out}`).toMatch(
      /"bruteForceProtected"\s*:\s*true/,
    );
  });

  test('30 bad-password attempts temporarily lock the account', async ({ baseURL }) => {
    const origin = baseURL ?? 'http://localhost:8090';
    const suffix = `${Date.now().toString(36)}${Math.floor(Math.random() * 1e4).toString(36)}`.toLowerCase();
    const user = `lockout-${suffix}`;
    const goodPass = `Pw-${suffix}-9`;
    let userId = '';
    try {
      // A throwaway verified user with a COMPLETE profile (first/last/email) and a
      // known PERMANENT password. The profile fields matter: without them the
      // realm's default user profile forces a VERIFY_PROFILE required action on
      // first login, which would stop the flow short of the redirect and make the
      // pre-lockout sanity login look like a failure.
      kcadm([
        'create', 'users', '-r', 'spawnly',
        '-s', `username=${user}`,
        '-s', 'enabled=true',
        '-s', 'emailVerified=true',
        '-s', 'firstName=Lock',
        '-s', 'lastName=Out',
        '-s', `email=${user}@example.test`,
      ]);
      kcadm(['set-password', '-r', 'spawnly', '--username', user, '-p', goodPass]);
      userId = kcadm(['get', 'users', '-r', 'spawnly', '-q', `username=${user}`, '--fields', 'id'])
        .match(/"id"\s*:\s*"([^"]+)"/)?.[1] ?? '';

      // Sanity: the correct password works BEFORE any failures.
      expect(await browserLoginAttempt(origin, user, goodPass), 'correct login should succeed before lockout').toBe(true);

      // Fire 30 consecutive bad-password attempts.
      for (let i = 0; i < 30; i++) {
        await browserLoginAttempt(origin, user, `wrong-${i}`);
      }

      // The account is now temporarily locked: even the CORRECT password is
      // rejected (Keycloak returns an error page, never the redirect).
      expect(await browserLoginAttempt(origin, user, goodPass), 'correct login must be rejected while the account is locked').toBe(false);
    } finally {
      if (userId) {
        try {
          kcadm(['delete', `users/${userId}`, '-r', 'spawnly']);
        } catch {
          /* best-effort cleanup */
        }
      }
    }
  });
});
