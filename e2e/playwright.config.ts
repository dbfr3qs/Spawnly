import { defineConfig, devices } from '@playwright/test';

// E2E suite for the Spawnly dashboard UI. The dashboard runs in Kind and is
// reached at http://localhost:8090 via `kubectl port-forward`, which the
// `webServer` block below owns (delegating to scripts/e2e.sh so kubeconfig is
// repaired identically inside the devcontainer and on a native host).
export default defineConfig({
  testDir: './tests',
  // One worker, no parallelism — this is load-bearing, do not raise it:
  //  (1) agents share one Kind cluster (pod scheduling would thrash under
  //      parallel spawns), and
  //  (2) anchor.spec and remote-agent.spec both drive acme's (and globex's)
  //      single attestation anchor through register→activate→clear. They are
  //      each `test.describe.serial` and restore the anchor to its seed (none)
  //      in afterAll, so run back-to-back they never collide — but only because
  //      a single worker guarantees they never overlap. Parallel workers would
  //      let one spec's activate race the other's clear on the SAME org anchor.
  fullyParallel: false,
  workers: 1,
  // Pod scheduling + image pulls dominate; keep generous ceilings.
  timeout: 180_000,
  expect: { timeout: 30_000 },
  reporter: [['list'], ['html', { open: 'never' }]],
  use: {
    baseURL: 'http://localhost:8090',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
  projects: [
    // Logs in once (alice/alice) and caches the session; everything else
    // depends on it so the specs run authenticated. See auth.setup.ts. Its
    // teardown (cleanup) runs after setup and all its dependents finish.
    { name: 'setup', testMatch: /auth\.setup\.ts/, teardown: 'cleanup' },
    {
      // Sweeps all agents + per-run org/anchor residue at the very end so the
      // cluster is left at its seed. Runs as the default alice@acme session, which
      // OWNS the bare-suite agent residue (the agent sweep is owner-scoped). The
      // org DELETE cascade needs the realm `admin` role, so global.teardown runs
      // sweepOrgs in a dedicated `root` context (alice is only an org admin now).
      name: 'cleanup',
      testMatch: /global\.teardown\.ts/,
      use: { storageState: 'playwright/.auth/user.json' },
    },
    {
      // headless is Playwright's default, but pin it so a stray HEADED/PWDEBUG
      // env or future edit can't pop a visible window during a run.
      name: 'chromium',
      // The org-subdomains suite (subdomains.spec.ts) is a SEPARATE project below,
      // run only against the local TLS mimic (make e2e-subdomains). Exclude it here
      // so the default `make e2e` (localhost:8090 mode) never picks it up.
      testIgnore: /subdomains\.spec\.ts/,
      use: {
        ...devices['Desktop Chrome'],
        headless: true,
        storageState: 'playwright/.auth/user.json',
      },
      dependencies: ['setup'],
    },
    // org-subdomains (Phase 4): proves the six subdomain e2e cases over the Phase-3
    // local TLS mimic (make tls-mimic → https://<slug>.localtest.me). GATED behind
    // SUBDOMAIN_E2E=1 so it only runs via `make e2e-subdomains`; a plain
    // `npx playwright test` (or `make e2e`) skips it entirely (empty project). It
    // does NOT depend on `setup` and carries NO cached storageState — cross-origin,
    // per-satellite session cookies are the subject under test, so every test
    // builds its own fresh context. Single worker like the rest (config-wide
    // fullyParallel:false / workers:1).
    ...(process.env.SUBDOMAIN_E2E === '1'
      ? [
          {
            name: 'subdomains' as const,
            testMatch: /subdomains\.spec\.ts/,
            use: {
              ...devices['Desktop Chrome'],
              headless: true,
              baseURL: 'https://localtest.me',
              ignoreHTTPSErrors: true,
            },
          },
        ]
      : []),
  ],
  webServer: {
    command: 'bash ../scripts/e2e.sh portforward',
    url: 'http://localhost:8090',
    // Reuse a port-forward the user already has running (e.g. `make dash`).
    reuseExistingServer: true,
    timeout: 120_000,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
