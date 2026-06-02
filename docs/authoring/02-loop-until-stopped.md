---
title: "Scenario 2 — Loop-Until-Stopped: the Queue Worker"
description: A long-lived agent that loops on a queue until deleted — lifecycle, heartbeats, and graceful shutdown.
---

# Scenario 2 — Loop-Until-Stopped: the Queue Worker

> **Prerequisite:** [Anatomy of an Agent](00-anatomy.md).
>
> **Reference implementation:** [`agents/weather-monitor`](../../agents/weather-monitor),
> whose template lives beside it at
> [`agents/weather-monitor/template.json`](../../agents/weather-monitor/template.json)
> (seeded by [`scripts/seed.sh`](../../scripts/seed.sh)). It is a long-lived agent that
> beats on an interval; the Queue Worker is the same lifecycle with real
> per-iteration work.

## The personality

The **Queue Worker** spins up and then *stays up*. On a fixed interval it polls a
work queue, processes each item it finds (calling a protected API per item),
emits a heartbeat plus a `item_processed` event for each one, and loops. It runs
indefinitely until the orchestrator deletes it. There is no natural end to its
work — stopping is an external decision.

This is the right shape for steady-state processing: a queue/inbox consumer, a
monitor/poller, a scheduler, or anything that should keep running and reacting.

## What "long-lived" changes

Set `runtimeSpec.lifecycle: "long-lived"` in the template. Two things follow,
both in [`internal/operator/reconciler.go`](../../internal/operator/reconciler.go):

1. **The operator creates a `<AGENT_ID>-svc` Service** (`buildService`). A pure
   queue worker may never receive inbound traffic, but the Service exists so the
   agent is addressable (and is required for the A2A child in Scenario 3).
2. **The pod exiting is no longer treated as completion** (`handleRunning` only
   auto-completes when `lifecycle != "long-lived"`). The workload stays
   `Running` until something deletes it.

## Shape of the code

A loop that runs until told to stop, with a `SIGTERM` handler for graceful
shutdown. Compare the trivial heartbeat in
[`agents/weather-monitor/heartbeat.mjs`](../../agents/weather-monitor/heartbeat.mjs),
which is exactly this minus the per-item work:

```ts
import { postEvent, TokenClient } from '@agent-platform/sdk';

const agentId     = process.env.AGENT_ID     ?? 'unknown';
const registryUrl = process.env.REGISTRY_URL ?? 'http://registry:8080';
const tenantId    = process.env.TENANT_ID    ?? 'default';
const apiUrl      = process.env.API_A_URL    ?? 'http://sample-api-a:8080';
const sidecarUrl  = process.env.SIDECAR_URL  ?? 'http://localhost:8089';
const intervalMs  = Number(process.env.POLL_INTERVAL_MS ?? 30_000);

const tokens = new TokenClient(sidecarUrl);
let stopped = false;

// One pass: drain whatever the queue currently holds.
async function processBatch(): Promise<void> {
  await postEvent(registryUrl, agentId, 'heartbeat', {
    status: 'running',
    timestamp: new Date().toISOString(),
  });

  const items = await pollQueue();           // your queue source
  for (const item of items) {
    if (stopped) break;
    // TokenClient caches per scope, so the per-item call is cheap across the loop.
    const token = await tokens.getToken('sample-api-a:write');
    const res = await fetch(`${apiUrl}/work`, {
      method: 'POST',
      headers: {
        Authorization: `Bearer ${token}`,
        'X-Tenant-ID': tenantId,
        'Content-Type': 'application/json',
      },
      body: JSON.stringify({ item }),
    });
    await postEvent(registryUrl, agentId, 'item_processed', {
      item,
      status: res.status,
      ok: res.ok,
    });
  }
}

async function main() {
  await postEvent(registryUrl, agentId, 'worker_started', { agentId, intervalMs });

  // Graceful shutdown: the operator sends SIGTERM when the workload is deleted.
  // Finish the current batch, emit a final event, exit 0 (recorded as Completed).
  process.on('SIGTERM', () => {
    stopped = true;
    void postEvent(registryUrl, agentId, 'worker_stopping', { reason: 'SIGTERM' })
      .finally(() => process.exit(0));
  });

  while (!stopped) {
    try {
      await processBatch();
    } catch (err) {
      await postEvent(registryUrl, agentId, 'worker_error', {
        error: err instanceof Error ? err.message : String(err),
      });
    }
    await new Promise((r) => setTimeout(r, intervalMs));
  }
}

main().catch((err) => {
  console.error('[queue-worker] fatal:', err);
  process.exit(1);
});
```

The heartbeat keeps the agent visibly alive on the dashboard between batches —
the same role `setInterval(beat, 30_000)` plays in `weather-monitor`. Make the
interval configurable through the template's `envDefaults` (e.g.
`POLL_INTERVAL_MS`), which the operator injects verbatim.

## The template

```json
{
  "agentType": "queue-worker",
  "version": "1.0.0",
  "status": "active",
  "meta": {"displayName": "Queue Worker", "description": "Long-lived agent that drains a queue on a loop"},
  "runtimeSpec": {
    "image": "agent-queue-worker:latest",
    "lifecycle": "long-lived",
    "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
    "envDefaults": {"LOG_LEVEL": "info", "POLL_INTERVAL_MS": "30000"}
  },
  "authzTemplate": {
    "spiceDbRelations": [
      {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
    ]
  }
}
```

The only difference from a job-and-exit template is `"lifecycle": "long-lived"`.

## How "told to stop" works

Deletion is the stop signal. The orchestrator's `DELETE /v1/agents/{id}` removes
the `AgentWorkload`; the operator's finalizer (`handleDeletion` in
[`reconciler.go`](../../internal/operator/reconciler.go)) tears down the pod and
records the outcome:

- A clean teardown (pod **not** in `PodFailed`) is recorded as **Completed** —
  so an orderly `SIGTERM` exit is *not* a failure.
- Only positive evidence of failure (a `PodFailed` pod, or one that never
  started) is recorded as **Failed**.

That distinction is why the `SIGTERM` handler above exits `0`: a long-lived agent
that is asked to stop should shut down gracefully and be remembered as
Completed, not Failed.

## Run it (using the seeded `weather-monitor`)

```bash
make demo   # port-forwards orchestrator :8080 + dashboard :8090

# Spawn the long-lived reference agent:
curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"agentType":"weather-monitor","tenantId":"tenant-1","userId":"user-1"}'
# -> {"workloadName":"weather-monitor-xxxxx"}

# It stays Running and a Service is created:
kubectl get agentworkload weather-monitor-xxxxx
kubectl get svc weather-monitor-xxxxx-svc

# Watch the heartbeat events accumulate:
curl -sf http://localhost:8080/v1/agents/weather-monitor-xxxxx/events | jq

# Tell it to stop (graceful teardown -> Completed):
curl -sf -X DELETE http://localhost:8080/v1/agents/weather-monitor-xxxxx
```

On the dashboard the agent stays in the running state, emitting heartbeats,
until you delete it — then it transitions to Completed.

## What this scenario teaches

- The `long-lived` lifecycle: Service creation + no auto-completion on exit.
- Heartbeats to stay observable between units of work.
- Graceful shutdown via `SIGTERM`, and why a clean stop is Completed not Failed.

**Next:** have one agent drive another in
[Scenario 3 — Parent → child](03-parent-and-child.md).
