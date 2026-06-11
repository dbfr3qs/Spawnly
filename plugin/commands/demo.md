---
description: Guided, narrated demo of Spawnly using the example agents — spawn, chains, revoke cascade, CIBA consent, handoff, tenancy
argument-hint: "[1-5 | scenario name]"
allowed-tools: Bash, Read
---

You are running a guided, narrated demo of Spawnly for someone seeing it for the
first time. Use the **spawnly-platform** skill for the API surface and concepts.
Requested scenario: **$ARGUMENTS** (empty = show the menu and let them choose).

## Setup (do this first, quietly)

- Ensure the cluster is up and seeded. If `kind get clusters` lacks
  `agent-platform` or core deployments aren't Ready, stop and tell them to run
  `/spawnly:up` first. If templates are empty, run `make reseed`.
- Ensure port-forwards exist for the orchestrator (`localhost:8080`) and
  dashboard (`localhost:8090`): if `pgrep -f 'port-forward.*8080'` is empty,
  start `kubectl port-forward svc/orchestrator 8080:8080` and
  `kubectl port-forward svc/dashboard 8090:8080` in the background.
- Tell them the dashboard is open at http://localhost:8090 to watch along.

## Scenario menu

1. **Hello world** (`worker`) — spawn one agent; watch it get its SPIFFE
   identity, mint a token, call the sample API, and complete. The token-minting
   path, end to end.
2. **Chains & real-time revocation** (`chain-worker`) — grow a 4-deep chain,
   revoke a middle node, watch its subtree flip to `work_denied` in real time
   while ancestors keep working, then resume.
3. **Human-in-the-loop consent (CIBA)** (`chain-worker`) — the first spawned
   link waits for consent; approve it; deeper links auto-approve from the stored
   consent; then revoke the consent and watch the next spawn re-prompt.
4. **Handoff / token-exchange** (`currency-converter`) — one agent type handing
   scoped authority to another via token-exchange.
5. **Tenancy** (`global-worker` vs a tenanted `worker`) — same code, tenant-
   scoped vs. global SVID; show the differing SPIFFE IDs.

If `$ARGUMENTS` names/numbers a scenario, run it. Otherwise show this menu and
ask which to run.

## How to run each beat

For the chosen scenario, narrate as you go — **explain → act → show → pause**:

- **Explain** what this demonstrates and why it matters (1-3 sentences), using
  the skill.
- **Act** via the real API:
  - spawn: `curl -s -X POST localhost:8080/spawn -H 'Content-Type: application/json'
    -d '{"userId":"user-1","tenantId":"tenant-1","agentType":"<type>","task":"<task>"}'`
    → capture `.workloadName`. (Omit `tenantId` for a global agent.)
  - observe: poll `GET localhost:8080/v1/agents/<id>/events` and surface the
    meaningful lifecycle events (`svid_fetched`, `token_issued`, `work_ok`,
    `consent_requested`, `consent_granted`, `work_denied`).
  - revoke/resume/consent: use the `/v1/agents/{id}/revoke|resume` and
    `/v1/consents` endpoints (scenario 2/3). For consent approval without a
    browser, walk them through the dashboard prompt at http://localhost:8090,
    or use the consent API.
- **Show** the result: the event timeline that proves what happened, and point
  at the corresponding card on the dashboard.
- **Pause** and offer: next beat / repeat / `/spawnly:explain <concept>` to go
  deeper / stop.

## Cleanup

When they're done, offer to tear down what the demo spawned (remember DELETE
does NOT cascade to chain children — delete the whole subtree, or note it). Keep
each beat short and concrete; the goal is an "aha", not a wall of logs.
