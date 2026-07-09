# Dashboard E2E tests

Browser-based end-to-end tests for the Spawnly dashboard UI, driven with
[Playwright](https://playwright.dev/). The tests open the real dashboard in a
headless browser and drive it the way a user would (spawn agents, revoke/resume,
chat), asserting on what the UI and the platform actually do.

## Scenarios

**Core / single-user:**

| Spec | What it covers |
| --- | --- |
| [`tests/smoke.spec.ts`](tests/smoke.spec.ts) | Log in and confirm the dashboard loads (fast smoke test, no LLM). |
| [`tests/auth.spec.ts`](tests/auth.spec.ts) | Logged-out redirect to Keycloak; wrong password rejected (identity-first two-screen form). |
| [`tests/ownership.spec.ts`](tests/ownership.spec.ts) | An agent's owner controls it; a different user gets 404s (deny = 404). |
| [`tests/chain-worker.spec.ts`](tests/chain-worker.spec.ts) | Spawn a `chain-worker` chain, confirm every node emits `work_ok`, **revoke a random child** and verify its subtree cascades to `work_denied` while ancestors keep passing, then **resume** and verify recovery. |
| [`tests/ciba-consent.spec.ts`](tests/ciba-consent.spec.ts) · [`tests/mobile-ciba.spec.ts`](tests/mobile-ciba.spec.ts) | Spawn-consent (CIBA) approve/deny from the dashboard and the mobile gateway. |
| [`tests/template-lifecycle.spec.ts`](tests/template-lifecycle.spec.ts) · [`tests/admin-agent-types.spec.ts`](tests/admin-agent-types.spec.ts) · [`tests/spawn-tenant-field.spec.ts`](tests/spawn-tenant-field.spec.ts) | Template CRUD, the admin agent-types screen, and the conditional `requiresTenant` spawn field (tenant primitive — unchanged by orgs). |
| [`tests/weather-chat.spec.ts`](tests/weather-chat.spec.ts) | Spawn the `weather-monitor`, send a chat message, and verify a non-empty reply. |
| [`tests/login-theme.spec.ts`](tests/login-theme.spec.ts) | The Spawnly-branded Keycloak login/registration theme renders. |

**Multi-tenancy (organizations) — multi-user, multi-org:**

| Spec | Criteria | What it covers |
| --- | --- | --- |
| [`tests/orgs.spec.ts`](tests/orgs.spec.ts) | 2–5 | Member sees the admin's agents read-only and cannot mutate them (UI absent AND direct API 404); org admin manages both; cross-org invisible; org-switch re-auth; admin org-less spawn; `nomad` hits the no-organization screen. |
| [`tests/membership.spec.ts`](tests/membership.spec.ts) | 8 | Org admin adds/removes/promotes members; both removal dispositions (`auto-revoke` / `orphan-to-admin`). |
| [`tests/org-templates.spec.ts`](tests/org-templates.spec.ts) | 7 | A template created in acme is invisible/unspawnable in globex regardless of `requiresTenant`; the seeded catalog is spawnable in both. |
| [`tests/signup.spec.ts`](tests/signup.spec.ts) | 9 | A brand-new user self-registers, lands on the no-org screen, creates an org, and spawns from the global catalog. |
| [`tests/anchor.spec.ts`](tests/anchor.spec.ts) | 10 | Anchor lifecycle + proof of control against the in-cluster **fake STS** (wrong account refused, right account activates; member read-only). |
| [`tests/remote-agent.spec.ts`](tests/remote-agent.spec.ts) | 11, 12 | A consumer-hosted (remote) agent self-registers via the org's STS anchor, mints, lists, and its next mint fails after a dashboard revoke; a cross-org STS identity is refused end-to-end. |

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
4. **Fake STS + remote-demo (for the attestation suites)** — deployed
   automatically: `make e2e` runs `make e2e-anchor-setup` first (see
   [Cluster-state assumptions](#cluster-state-assumptions)). You do not run it
   by hand.

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

## Seeded users & orgs (the multi-user matrix)

The org suites drive several seeded identities (`deploy/keycloak/users.tf` +
`scripts/reseed-orgs.sh`; **password == username** for every demo user). The
realm `admin` role — the platform master admin, with the org-less Global view —
is held ONLY by `root`; `alice` is an ORG admin (a SpiceDB tuple), not the realm
admin:

| User | Realm role | Org membership |
| --- | --- | --- |
| `root` | `admin` (platform) | **no org** (the platform master admin / Global) |
| `alice` | — | org **admin** of `acme` |
| `viewer` | — | **member** of `acme` |
| `bob` | — | org **admin** of `globex` (cross-org isolation) |
| `carol` | — | **member** of both `acme` and `globex` (silent org switch) |
| `nomad` | — | **no org** (drives the "no organization" screen) |

Helpers live in [`helpers/auth.ts`](helpers/auth.ts): `login(page, user, pass,
org?)` drives Keycloak's identity-first (two-screen) login and, when an `org`
is given, starts at `/signin?org=<alias>` so the first token already carries
that single-org scope; `contextFor(browser, user, org?)` logs a user into a
**fresh** context (no shared SSO cookies) and caches the state to
`playwright/.auth/<user>[-<org>].json` — this is how the two-session matrix
specs (a member and an admin at once) get isolated sessions. `nomad` cannot
browser-login if created via the admin API (required password reset), so it is
seeded declaratively.

## Cluster-state assumptions

The suite assumes a bootstrapped cluster and manages the attestation substrate
itself. Two facts about cluster state matter:

- **`make bootstrap` resets Keycloak's STS trust.** A fresh bootstrap deploys
  Keycloak/registry with **no** `STS_TRUSTED_ENDPOINT`, so nothing trusts the
  fake STS yet, and it seeds the demo orgs (`acme`, `globex`).
- **`make e2e-anchor-setup` (a prereq of `make e2e`) re-wires it.**
  [`scripts/deploy-fake-sts.sh`](../scripts/deploy-fake-sts.sh) builds+loads the
  `fake-sts` and `remote-demo` images, applies the standalone `fake-sts`
  manifest (NOT in the kustomize base — e2e only), and points **both** the
  registry and Keycloak at `http://fake-sts:8080` as their sole trusted STS
  endpoint. On kind, Keycloak uses the in-memory dev-file DB, so that env change
  **restarts Keycloak and wipes the realm** — the script re-applies it via
  [`scripts/apply-realm.sh`](../scripts/apply-realm.sh), which re-runs the realm
  Terraform *and* reseeds templates + demo orgs. Every step is idempotent and
  gated on the env value actually changing, so re-running `make e2e` is cheap
  after the first run. A real deploy never sets `STS_TRUSTED_ENDPOINT`, so this
  whole path is e2e-only and un-triggerable in production.

The upshot: just run `make e2e`. On the first run after a bootstrap it will pause
to build the fake-STS images and rebuild the realm; subsequent runs skip that.

## Notes

- Each test cleans up the agents it spawns in `afterEach`, so runs are isolated.
- Tests run serially (`workers: 1`, `fullyParallel: false`) — spawning many
  agents in parallel would contend on the shared cluster, and `anchor.spec` /
  `remote-agent.spec` each drive a demo org's single attestation anchor, so they
  must never overlap (see the comment in `playwright.config.ts`).
- The end-of-suite teardown ([`tests/global.teardown.ts`](tests/global.teardown.ts))
  sweeps every kind of residue so repeated runs on one cluster stay isolated:
  all remaining agents, the `remote-demo` pods, the `org-<suffix>` orgs that
  `signup.spec` leaves behind, and the demo orgs' attestation anchors (back to
  seed). The signup Keycloak **user** accounts persist (there is no user-delete
  API surface); they are inert (unique per run, one-org-per-user) and harmless.
