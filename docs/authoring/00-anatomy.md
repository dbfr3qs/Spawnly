---
title: Anatomy of an Agent
description: The shared contract every agent satisfies — platform-injected identity and environment, the SDK, and the six-step build, register, spawn, and observe path.
---

# Anatomy of an Agent

This is the shared reference for authoring agents on the platform. Read it once;
the three scenario guides build directly on it:

1. [Job-and-exit](01-job-and-exit.md)
2. [Loop-until-stopped — Queue Worker](02-loop-until-stopped.md)
3. [Parent → child — consent-gated fan-out](03-parent-and-child.md)

Scenarios 2 and 3 use agents already in this repo as their working reference
implementations (`weather-monitor`, `chain-worker`, and the `travel-planner` →
specialist fan-out), so you can read real, running code alongside the
explanation. Scenario 1 is the simplest *shape* of the path below — there is no
dedicated minimal example agent, so it leans on this anatomy for the mechanics.

---

## What the platform does for you

You write the *behaviour*. The platform owns *identity, authorisation, and
lifecycle*. Concretely, when your agent pod starts, the operator injects a
**native sidecar init-container** ([`internal/operator/reconciler.go`](../../internal/operator/reconciler.go) →
`buildPod`) that:

1. Mounts the SPIRE CSI volume and fetches the pod's JWT-SVID.
2. Self-registers the agent with the registry (writing the SpiceDB relations
   from the template's `authzTemplate`).
3. Exposes a **local token endpoint at `http://localhost:8089/token`**.

Your code never speaks to SPIRE or IdentityServer directly. To call a protected
API you ask the sidecar for a scoped token via the SDK's `TokenClient`:

```ts
import { TokenClient } from '@spawnly/sdk';

const tokens = new TokenClient(); // defaults to http://localhost:8089
const accessToken = await tokens.getToken('sample-api-a:read');
```

> The sidecar listens on `:8089` only *after* it has fetched its SVID and
> registered, while your container may start sooner. `TokenClient` handles that
> startup race for you — it retries on connection errors / 5xx until the sidecar
> is ready, and fails fast on a 4xx (bad scope or policy denial). See
> [The SDK](#the-sdk) for the full token API.

## Environment the operator injects

Set on every agent container ([`reconciler.go`](../../internal/operator/reconciler.go) → `buildPod`).
Read them with `process.env`:

| Variable | Meaning |
|----------|---------|
| `AGENT_ID` | This agent's canonical id (also the workload/pod name). Use it for events and as the A2A service host (`<AGENT_ID>-svc`). |
| `TENANT_ID` / `USER_ID` | Tenant and user the agent acts for. `TENANT_ID` is **empty for a global (tenant-agnostic) agent** — send `X-Tenant-ID` on protected calls only when it is set (the SDK's authenticated fetch does this for you). See [tenanted vs global](04-defining-a-template.md#tenanted-vs-global-agents). |
| `PARENT_ID` | Set when this agent was spawned by another agent (empty otherwise). |
| `REGISTRY_URL` | Post lifecycle events here. |
| `ORCHESTRATOR_URL` | Spawn / kill other agents here. |
| `IS_TOKEN_URL` | IdentityServer token URL (used by the sidecar; rarely needed directly). |
| `SAMPLE_API_URL`, `API_A_URL`, `API_B_URL` | Base URLs of the protected sample APIs. |
| `TASK` | Free-text task string passed at spawn time (optional). |
| `AI_PROVIDER`, `AI_API_KEY`, `AI_MODEL` | LLM provider config, sourced from the `ai-provider` Secret ([`deploy/secrets/ai-provider.yaml`](../../deploy/secrets/ai-provider.yaml)). |
| *(template `envDefaults`)* | Any extra key/values declared in the template are injected verbatim. |

## The SDK

Shared helpers live in [`@spawnly/sdk`](../../sdks/typescript/src/index.ts). The
ones you will use:

- **`TokenClient`** — wraps the sidecar's `/token` endpoint (the platform's
  neutral token contract), with the startup-retry and caching built in:
  - `new TokenClient(baseUrl?)` — defaults to `http://localhost:8089`.
  - `getToken(scope, { audience? })` — a client-credentials token for `scope`,
    cached per `scope|audience`. Pass `audience` to target a resource or mint a
    delegation token (`{ audience: 'delegation' }`). For what IdentityServer does
    with the SVID to produce that token, see
    [How an agent's token is minted](/internals/token-minting).
  - `exchangeToken({ subjectToken, audience, scope })` — RFC 8693 token-exchange
    (a child exchanging a delegation token from its parent). Never cached.
  - `createAuthenticatedFetch(baseUrl, scope)` — a `fetch` that attaches a
    `Bearer` token for `scope` automatically.
- **`postEvent(registryUrl, agentId, type, payload)`** — append a lifecycle
  event. Never throws. This is how anything you do shows up on the dashboard.
- **`instrumentFlue(ctx, registryUrl, agentId)`** — tap a Flue runtime context
  and forward LLM turns / tool calls / errors to the event stream as a neutral,
  framework-agnostic vocabulary. Call it once after `createFlueContext`.
- **`promptTimeoutSignal(ms)`** — an `AbortSignal` to bound an LLM prompt.

The same neutral contract is available in **Go** for non-Flue workloads:
[`sdks/go`](../../sdks/go) (`github.com/spawnly/sdk-go`) mirrors `TokenClient`,
an authenticated HTTP client, the tenant-header helper, and `postEvent` — minus
the Flue-specific `instrumentFlue` / `promptTimeoutSignal` (Go uses `context`
deadlines instead). No example Go agent ships at the moment, but the contract is
identical to the TypeScript SDK's.

Keep the dependency direction in mind: the SDK stays framework-agnostic and
depends on the platform's neutral contract, never the reverse. Don't pull
platform internals into agent code; lean on the SDK and the env contract above.

---

## The six-step path from scratch

The process is identical for all three scenarios. **The only field that changes
the scenario is `runtimeSpec.lifecycle` in the template** (see below).

### 1. Write the agent under `agents/<name>/`

A TypeScript project depending on `@spawnly/sdk` and `@flue/runtime` — or, for a
non-Flue workload, a Go module depending on `github.com/spawnly/sdk-go`.
Use one of the reference agents as a starting skeleton:

| Scenario | Reference agent | Shape |
|----------|-----------------|-------|
| Job-and-exit | *(no dedicated example; see [01](01-job-and-exit.md))* | `main()` runs, then the process exits |
| Loop-until-stopped | [`agents/weather-monitor`](../../agents/weather-monitor), [`agents/chain-worker`](../../agents/chain-worker) | `setInterval` / loop until terminated |
| Parent → child | [`agents/travel-planner`](../../agents/travel-planner) + the [`travel-specialist`](../../agents/travel-specialist) specialists | parent fans out; each child is a consent-gated A2A/MCP-client server |

### 2. Add a Dockerfile build target

Add a multi-stage block to the [`Dockerfile`](../../Dockerfile) following the
`build-<name>-node` → final `agent-<name>` pattern used by `weather-monitor`,
`chain-worker`, and `travel-planner`. Every Node agent image copies the compiled
shared SDK from the `build-ts-sdk` stage. (A Go agent would follow a parallel
`build-<name>` → `<name>` stage pattern instead, building its own module.)

### 3. Build and load the image into Kind

```bash
make kind-load
```

### 4. Register a template

The registry is an in-memory template + agent store. Save your agent type as a
`template.json` next to your agent (`agents/<name>/template.json`) — it is
discovered and seeded by [`scripts/seed.sh`](../../scripts/seed.sh) (`make reseed`)
so it survives a registry restart. The file is just the `POST /v1/templates` body:

```bash
# agents/<name>/template.json
curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "<name>",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "...", "description": "..."},
    "runtimeSpec": {
      "image": "agent-<name>:latest",
      "lifecycle": "short-lived",        // or "long-lived" — see below
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
```

`{{tenant_id}}` and `{{agent_id}}` are expanded by the registry at registration
time. `seed.sh` port-forwards the registry to `localhost:18080`. For the full
field-by-field schema see [04 — Defining a Template](04-defining-a-template.md);
for what `authzTemplate`/`delegation` authorise see [05 — Defining Policy](05-defining-policy.md).

> **Prefer config-as-code?** The same template can be managed declaratively with
> the Terraform provider instead of a raw `POST` — see
> [Config-as-code with Terraform](../operating/config-as-code.md).

#### `lifecycle` — the one switch that defines the scenario

| `lifecycle` | Operator behaviour | Used by |
|-------------|--------------------|---------|
| `short-lived` (or omitted) | When the pod exits `0`, the workload is marked **Completed**. No Service is created. | Scenario 1 |
| `long-lived` | The operator also creates a `<AGENT_ID>-svc` Service and does **not** auto-complete when the pod exits. | Scenarios 2 & 3 (child) |

See [`reconciler.go`](../../internal/operator/reconciler.go) — `handleRunning`
(completion) and `buildService` (the `-svc` Service).

### 5. Spawn

```bash
curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"agentType":"<name>","tenantId":"tenant-1","userId":"user-1","task":"..."}'
# -> {"workloadName":"<name>-xxxxx"}
```

The orchestrator reads `lifecycle` from the template, writes an `AgentWorkload`
CRD, and the operator takes over. `parentId` is added automatically when one
agent spawns another (Scenario 3).

### 6. Observe

```bash
# Port-forward orchestrator (:8080) and dashboard (:8090):
make demo            # or: kubectl port-forward svc/orchestrator 8080:8080 &
                     #     kubectl port-forward svc/dashboard 8090:8080 &

curl -sf http://localhost:8080/v1/agents/<workloadName>/events | jq
kubectl get agentworkloads -w
```

Open **http://localhost:8090** to watch the lifecycle timeline — decoded JWTs,
SpiceDB relations, API calls, and every `postEvent` your agent emits.

---

## Lifecycle event sequence (reference)

The standard sequence for a short-lived agent (from the [top-level README](../../README.md)):

`workload_created` → `pod_created` → `registry_record_created` →
`spicedb_relations_written` → `svid_acquired` → `registry_self_registered` →
`token_requested` → `token_received` → `task_dispatched` → `task_result` →
`agent_completed`.

Everything between registration and completion is your agent's behaviour — and
that is what the three scenario guides cover.
