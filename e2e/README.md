# Dashboard E2E tests

Browser-based end-to-end tests for the Spawnly dashboard UI, driven with
[Playwright](https://playwright.dev/). The tests open the real dashboard in a
headless browser and drive it the way a user would (spawn agents, revoke/resume,
chat), asserting on what the UI and the platform actually do.

## Scenarios

| Spec | What it covers |
| --- | --- |
| [`tests/global-worker.spec.ts`](tests/global-worker.spec.ts) | Spawn a `global-worker` and confirm it runs to **completion** (fast smoke test, no LLM). |
| [`tests/chain-worker.spec.ts`](tests/chain-worker.spec.ts) | Spawn a `chain-worker` chain, confirm every node emits `work_ok`, **revoke a random child** and verify its subtree cascades to `work_denied` while ancestors keep passing, then **resume** and verify recovery. |
| [`tests/weather-chat.spec.ts`](tests/weather-chat.spec.ts) | Spawn the `weather-monitor`, send a chat message, and verify a non-empty reply. |

Actions go through the UI; event-state assertions read the timeline API (the
same data the page polls) so they don't race the UI's re-render loop.

## Prerequisites

1. **A bootstrapped cluster** — the suite assumes the platform is already
   running in Kind; it does not build it:
   ```sh
   make bootstrap
   ```
2. **An AI provider key** for the LLM-backed agents (weather chat). bootstrap
   reads it from the repo-root `.env` (`ANTHROPIC_API_KEY`, or set
   `AI_PROVIDER=openai` + `OPENAI_API_KEY`). See [`.env.example`](../.env.example).
3. **One-time Playwright install** (downloads Chromium):
   ```sh
   make e2e-setup
   ```

## Running

From the repo root:

```sh
make e2e
```

This works **identically inside the devcontainer and on a native host** — the
only difference between the two is the Kind kubeconfig, which the port-forward
helper repairs via `make kubeconfig` before forwarding. Playwright owns the
dashboard port-forward (`localhost:8090`) for the duration of the run; if you
already have one up (e.g. `make dash`), it is reused.

Useful variants (run inside `e2e/`):

```sh
npx playwright test chain-worker          # one spec
npx playwright test --repeat-each=3        # stress a spec for flakiness
npx playwright test --headed               # watch it drive a real browser
npm run report                             # open the last HTML report
```

## How it fits together

- [`playwright.config.ts`](playwright.config.ts) — `baseURL` is the dashboard at
  `localhost:8090`; a single worker (agents share one cluster); generous
  timeouts (pod scheduling + LLM latency); the `webServer` block owns the
  port-forward.
- [`../scripts/e2e.sh`](../scripts/e2e.sh) — `portforward` subcommand: repairs
  kubeconfig, then `kubectl port-forward svc/dashboard 8090:8080`.
- [`helpers/`](helpers) — `dashboard.ts` (spawn, waitForStatus, listAgents),
  `events.ts` (timeline reads + `waitForEventType`), `cleanup.ts` (cascade-aware
  teardown).

The dashboard UI exposes stable hooks the tests rely on: `data-testid` on the
status badge and the revoke/resume/kill buttons, and `data-event-type` on each
event row. Keep those when editing
[`cmd/dashboard/static/index.html`](../cmd/dashboard/static/index.html).

## Notes

- Each test cleans up the agents it spawns in `afterEach`, so runs are isolated.
- Tests run serially; spawning many agents in parallel would contend on the
  shared cluster.
