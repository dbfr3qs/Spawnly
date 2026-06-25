---
title: "Scenario 1 — Job-and-Exit: the Price Reporter"
description: The simplest agent — acquire a scoped token, call a protected API, emit a result event, and exit cleanly.
---

# Scenario 1 — Job-and-Exit: the Price Reporter

> **Prerequisite:** [Anatomy of an Agent](00-anatomy.md).
>
> **Reference implementation:** none ships today — this is the simplest *shape* of
> the [six-step path](00-anatomy.md#the-six-step-path-from-scratch), so the guide
> builds a hypothetical `price-reporter` from scratch. (`travel-planner` is the
> closest runnable one-shot, but it is a fan-out orchestrator — [Scenario 3](03-parent-and-child.md).)

## The personality

The **Price Reporter** spins up, asks the sidecar for a scoped token, calls a
protected price API for a watchlist, formats a short summary, emits it as a
lifecycle event, and exits cleanly. It holds no state, serves no traffic, and
has no Service. When its `main()` returns, the pod exits `0` and the operator
marks the workload **Completed**.

This is the canonical "do one job and disappear" agent — the right shape for
report generation, a one-shot data pull, an ETL step, or any task with a clear
beginning and end.

## Shape of the code

A single `main()` that runs top to bottom and then returns. No server, no loop.

```ts
import { postEvent, TokenClient } from '@spawnly/sdk';

const agentId     = process.env.AGENT_ID     ?? 'unknown';
const registryUrl = process.env.REGISTRY_URL ?? 'http://registry:8080';
const tenantId    = process.env.TENANT_ID    ?? 'default';
const apiUrl      = process.env.API_A_URL    ?? 'http://sample-api-a:8080';
const sidecarUrl  = process.env.SIDECAR_URL  ?? 'http://localhost:8089';

// The SDK's TokenClient talks to the local sidecar's /token endpoint. It handles
// the sidecar-not-ready retry (it binds :8089 slightly after the container
// starts) and caches tokens per scope, so your code just asks for a scope.
const tokens = new TokenClient(sidecarUrl);

async function main() {
  await postEvent(registryUrl, agentId, 'reporter_started', { agentId });

  // 1. Acquire a scoped token for the protected price API.
  const token = await tokens.getToken('sample-api-a:read');

  // 2. Call the protected API — pass the tenant header.
  const res = await fetch(`${apiUrl}/work`, {
    method: 'GET',
    headers: { Authorization: `Bearer ${token}`, 'X-Tenant-ID': tenantId },
  });
  const body = await res.json().catch(() => ({}));

  // 3. Emit the result as a lifecycle event (this is what the dashboard shows).
  await postEvent(registryUrl, agentId, 'price_report', {
    status: res.status,
    ok: res.ok,
    report: body,
  });

  // 4. Return. The process exits, the pod completes, the workload -> Completed.
  console.log('[price-reporter] done');
}

main().catch(async (err) => {
  await postEvent(registryUrl, agentId, 'agent_error', {
    error: err instanceof Error ? err.message : String(err),
  });
  process.exit(1);
});
```

The snippet above is TypeScript, but the same token → protected-call shape is
available in Go on the Go SDK (`github.com/spawnly/sdk-go`, [`sdks/go`](../../sdks/go)).
Its `AuthenticatedClient` attaches the `Bearer` token and `X-Tenant-ID` for you,
so the core is just:

```go
tc := spawnly.NewTokenClient()
client := spawnly.NewAuthenticatedClient(sampleAPIURL, scope, tc, spawnly.WithTenantID(tenantID))

// Authorization + X-Tenant-ID are injected by the client; relative path resolves against sampleAPIURL.
resp, err := client.Post(ctx, "/task", body)
```

### Optional: add an LLM summarisation step

If the report benefits from natural-language summarisation, build a Flue context
and prompt the model once, then `instrumentFlue` so the turn shows up on the
dashboard (see [`agents/weather-monitor`](../../agents/weather-monitor) for a Flue
agent). For a pure data job, skip the LLM entirely — a job-and-exit agent does
not need one.

## The template

No `lifecycle` field — it defaults to short-lived, so the operator marks the
workload Completed on exit and creates no Service:

```json
{
  "agentType": "price-reporter",
  "version": "1.0.0",
  "status": "active",
  "meta": {"displayName": "Price Reporter", "description": "Pulls prices, reports, and exits"},
  "runtimeSpec": {
    "image": "agent-price-reporter:latest",
    "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
    "envDefaults": {"LOG_LEVEL": "info"}
  },
  "authzTemplate": {
    "spiceDbRelations": [
      {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
    ]
  }
}
```

The `authzTemplate` grants `tenant:T#agent@agent:X`, which is what lets the
sidecar mint a token the price API will accept (the API checks the SpiceDB
`work_on` permission before serving the request).

## Run it

No minimal job-and-exit agent is seeded, so run *your own*: build + load the
image, seed the `price-reporter` template, then spawn it — the
[six-step path](00-anatomy.md#the-six-step-path-from-scratch) in 00 covers each
command. The spawn + observe loop is:

```bash
# Port-forward orchestrator :8080 + dashboard :8090:
make demo

curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"agentType":"price-reporter","tenantId":"tenant-1","userId":"user-1","task":"price snapshot"}'
# -> {"workloadName":"price-reporter-xxxxx"}

# Watch it reach Completed:
kubectl get agentworkload price-reporter-xxxxx -w

# Inspect the event timeline (your price_report event appears near the end):
curl -sf http://localhost:8080/v1/agents/price-reporter-xxxxx/events | jq
```

On the dashboard (http://localhost:8090) the agent appears, walks through
registration → token → API call → your `price_report` event → `agent_completed`,
and then the pod is gone.

## What this scenario teaches

- The minimal viable agent: token → protected call → event → exit.
- Short-lived completion semantics (`lifecycle` omitted ⇒ Completed on exit `0`).
- That an agent needs **no Service and no LLM** to be useful.

**Next:** keep the agent alive across many jobs in
[Scenario 2 — Loop-until-stopped](02-loop-until-stopped.md).
