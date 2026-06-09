import { defineConfig, devices } from '@playwright/test';

// E2E suite for the Spawnly dashboard UI. The dashboard runs in Kind and is
// reached at http://localhost:8090 via `kubectl port-forward`, which the
// `webServer` block below owns (delegating to scripts/e2e.sh so kubeconfig is
// repaired identically inside the devcontainer and on a native host).
export default defineConfig({
  testDir: './tests',
  // Agents share one cluster; serialize to avoid cross-test contention.
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
    { name: 'chromium', use: { ...devices['Desktop Chrome'] } },
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
