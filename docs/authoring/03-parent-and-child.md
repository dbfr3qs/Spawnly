---
title: "Scenario 3 — Parent → Child: Trip Planner & Currency Converter"
description: Agent-to-agent orchestration over A2A, with delegated and attenuated authority — plus an own-authority handoff variant.
---

# Scenario 3 — Parent → Child: Trip Planner & Currency Converter

> **Prerequisite:** [Anatomy of an Agent](00-anatomy.md). This scenario builds on
> both earlier ones — the parent does a job and exits (Scenario 1), the child is
> a long-lived A2A server (Scenario 2 lifecycle).
>
> **Reference implementations:** [`agents/parent-agent`](../../agents/parent-agent)
> and [`agents/child-agent`](../../agents/child-agent), whose templates live beside
> them as `template.json` (seeded by [`scripts/seed.sh`](../../scripts/seed.sh)).

## The personalities

The **Trip Planner** (parent) spins up and does its own work, then needs a narrow
sub-task done — converting an amount between currencies. Rather than acquire that
capability itself, it **spawns a Currency Converter** (child), **delegates a
read-only, narrowly-scoped task** to it over A2A, collects the result, and tears
the child down. Then the parent reports and exits.

The **Currency Converter** (child) is a long-lived A2A server. On request it
**exchanges the delegation token it was handed** for a token scoped to the
currency API and performs the conversion. Crucially, the token it receives is
*attenuated*: it can **read** the currency API but **cannot write** it — the
platform enforces least privilege across the agent boundary.

This is the shape for any "lead agent that fans work out to specialists":
research lead → researcher, planner → tool-specialist, orchestrator → worker —
with delegation that hands down *only* the authority the sub-task needs.

## The two halves

### Parent — an orchestrator that exits

The parent is a job-and-exit agent (Scenario 1) whose "job" is to drive a child.
It exposes four tools to its LLM session, all implemented in
[`agents/parent-agent/src/index.ts`](../../agents/parent-agent/src/index.ts):

| Tool | Does |
|------|------|
| `spawn_child_agent` | `POST /spawn` on the orchestrator with `agentType: "currency-converter"`, `parentId: AGENT_ID`. Returns the child's id. |
| `wait_for_child_ready` | Polls `http://<childId>-svc:8080/.well-known/agent.json` until the child's A2A server answers. |
| `call_child_agent` | Opens an A2A client to `http://<childId>-svc:8080` and `sendMessage(...)`, carrying the **delegation token in message metadata**. |
| `kill_child_agent` | `DELETE /v1/agents/<childId>` on the orchestrator. |

Before handing control to the LLM, the parent does its delegation setup
*deterministically* (so the acceptance path doesn't depend on model behaviour):

```ts
// 1. Do the parent's own privileged work (read+write on its own API).
await callApiADirect();                       // POST /work on API-A

// 2. Mint a delegation token, attenuated to read-only on the child's API.
delegationToken = await getSidecarToken({
  audience: 'delegation',
  scope: 'sample-api-b:read',                 // read only — no write
});
await postEvent(registryUrl, agentId, 'delegation_token_minted', { scope: 'sample-api-b:read' });
```

The token is then passed to the child via A2A message metadata
(`metadata: { delegationToken }`) in `call_child_agent`. See
[`parent-agent/src/index.ts`](../../agents/parent-agent/src/index.ts) lines around
`mintDelegationToken()` and `callChildAgent`.

The parent's template is short-lived (it exits after the round-trip) **and**
carries the delegation policy:

```json
{
  "agentType": "trip-planner",
  "version": "1.0.0",
  "status": "active",
  "meta": {"displayName": "Trip Planner", "description": "Spawns a currency converter, delegates a read-only conversion, then exits"},
  "runtimeSpec": {"image": "agent-trip-planner:latest", "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"}, "envDefaults": {}},
  "authzTemplate": {
    "spiceDbRelations": [
      {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
    ]
  },
  "delegation": {"allowedChildTypes": ["currency-converter"], "grantableScopes": ["sample-api-b:read"], "maxDepth": 3}
}
```

The `delegation` block is the policy gate: the parent may only spawn the listed
child types and may only grant the listed scopes. It is what makes
`scope: 'sample-api-b:read'` legal and `sample-api-b:write` impossible to grant.
See [05 — Defining Policy](05-defining-policy.md#part-2--delegation) for the full
delegation model and enforcement points.

### Child — a long-lived A2A server

The child is a long-lived agent (so it gets a `<id>-svc` Service) that runs an
A2A server. The full implementation is
[`agents/child-agent/src/index.ts`](../../agents/child-agent/src/index.ts); the
essentials:

1. **Publishes an agent card** at `/.well-known/agent.json` (how the parent's
   `wait_for_child_ready` discovers it) describing its skill.
2. **On each message, extracts the delegation token** from message metadata
   (`extractDelegationToken`).
3. **Exchanges it** at the sidecar for a token scoped to the currency API
   (`exchangeDelegationToken`, RFC 8693 — passing `subject_token=<delegation>`),
   then calls the API and replies over A2A.

The attenuation is the headline. With the read-only delegated token the child
sees:

```ts
// GET succeeds — the delegated scope permits reads.
const read  = await fetch(`${apiBUrl}/work`, { method: 'GET',  headers: { Authorization: `Bearer ${exchanged}`, 'X-Tenant-ID': tenantId }});
// status 200

// POST is denied — the same token cannot write. Least privilege, enforced.
const write = await fetch(`${apiBUrl}/work`, { method: 'POST', headers: { Authorization: `Bearer ${exchanged}`, 'X-Tenant-ID': tenantId }});
// status 403  (expected)
```

The child's template is long-lived (it must be reachable as a Service):

```json
{
  "agentType": "currency-converter",
  "version": "1.0.0",
  "status": "active",
  "meta": {"displayName": "Currency Converter", "description": "Long-lived A2A server that performs a delegated, read-only conversion"},
  "runtimeSpec": {"image": "agent-currency-converter:latest", "lifecycle": "long-lived", "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"}, "envDefaults": {}},
  "authzTemplate": {
    "spiceDbRelations": [
      {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
    ]
  }
}
```

## The end-to-end flow

```
Trip Planner (parent)                         Currency Converter (child)
  │  callApiADirect()  (own read+write work)
  │  mint delegation token (sample-api-b:read)
  │  spawn_child_agent ───────────────────────►  pod + <id>-svc created (long-lived)
  │  wait_for_child_ready  ── GET agent.json ──►  A2A server ready
  │  call_child_agent  ── A2A msg + token ─────►  extract + exchange token
  │                                               GET  API-B  -> 200  (read ok)
  │                                               POST API-B  -> 403  (write denied)
  │  ◄──────────── A2A reply (result) ──────────  reply over A2A
  │  kill_child_agent ── DELETE /v1/agents ────►  pod torn down
  │  report + exit (Completed)
```

## Run it (using the seeded `parent-agent` / `child-agent`)

The parent spawns the child itself — you only spawn the parent.

```bash
make demo   # port-forwards orchestrator :8080 + dashboard :8090

curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"agentType":"parent-agent","tenantId":"tenant-1","userId":"user-1"}'
# -> {"workloadName":"parent-agent-xxxxx"}

# Watch both the parent and the child it spawns:
kubectl get agentworkloads -w

# Parent timeline: delegation_token_minted, then the child round-trip:
curl -sf http://localhost:8080/v1/agents/parent-agent-xxxxx/events | jq

# Child timeline: delegation_exchange, api_b_call (200), api_b_write_denied (403):
curl -sf http://localhost:8080/v1/agents/child-agent-yyyyy/events | jq
```

> This scenario needs the `ai-provider` Secret populated (the parent and child
> each run an LLM session). Set `AI_API_KEY` in `.env` before `make bootstrap`
> — see [`.env.example`](../../.env.example).

On the dashboard you'll see two agents appear: the parent, and the child it
spawns; the child's `api_b_write_denied` event (status 403) is the visible proof
that delegation handed down read access only.

## Variant: handing off *without* delegation (own-authority child)

Delegation is the right model when the child acts **on the user's behalf** with a
slice of the parent's authority. But sometimes you just want the parent to *hand
work to* a child that does its **own** thing with its **own** permissions —
possibly completely different from the parent's. That needs *less* config, not
more: the orchestration/A2A scaffolding is identical, and you simply drop the
delegation machinery.

> See [05 — Defining Policy](05-defining-policy.md) for the own-authority vs
> delegated-authority distinction this variant rests on.

**What you remove vs the delegation flow above:**

| Piece | Delegation flow | Own-authority handoff |
|-------|-----------------|-----------------------|
| Parent `delegation.allowedChildTypes` | required | **keep** — still gates the spawn (deny-by-default; see caveat 2) |
| Parent `delegation.grantableScopes` / `maxDepth` | required | **omit** — no authority flows down |
| Parent mints a delegation token (`audience=delegation`) | yes | **drop** |
| Token passed over A2A metadata | yes | **drop** — the A2A call carries only the task |
| Child exchanges `subject_token` | yes | **drop** — child calls `/token?scope=…` (client_credentials), like a [Scenario 1](01-job-and-exit.md) agent |
| IS client `token-exchange` grant | parent + child | **drop** — both need only `client_credentials` |

**What stays the same:** the parent still `spawn`s the child with `parentId`,
waits on `<id>-svc`, calls it over A2A, and kills it; the child is still
long-lived (so it gets a Service) and still self-registers with its own
`authzTemplate`.

**Where the child's authority comes from:** entirely its own config, with no
reference to the parent — its template's `authzTemplate` (SpiceDB relations) plus
its IdentityServer client `AllowedScopes`. To give the child *different,
non-overlapping* permissions, set them directly on the child type. The trimmed
parent template keeps `allowedChildTypes` (so the spawn is permitted) but drops
the scope-flow fields:

```json
{
  "agentType": "trip-planner",
  "version": "1.0.0",
  "status": "active",
  "meta": {"displayName": "Trip Planner", "description": "Spawns a specialist child and hands off a task; no delegated authority"},
  "runtimeSpec": {"image": "agent-trip-planner:latest", "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"}, "envDefaults": {}},
  "authzTemplate": {
    "spiceDbRelations": [
      {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
    ]
  },
  "delegation": {"allowedChildTypes": ["currency-converter"]}
}
```

**Two caveats to weigh before choosing this model:**

1. **"On whose behalf" changes.** A delegated token keeps `sub` = the user down
   the whole chain; an own-authority token has `sub` = the child. The lineage is
   still recorded in the registry (`parentId` / [`/v1/agents/{id}/chain`](../../cmd/registry/main.go#L414)),
   but the *token itself* no longer asserts "acting for user-1." If a resource
   server must authorize on the originating user, delegation is the only path
   that carries it.
2. **The parent→child edge is governed at spawn (deny-by-default).** Even without
   a token-exchange, the orchestrator checks the parent template's
   `allowedChildTypes` at spawn time and rejects (`403`) a child type the parent
   doesn't list — so a parent that hands off to a child must declare it, exactly
   as the delegation flow already does.

## What this scenario teaches

- Agent-to-agent orchestration: spawn → discover (`<id>-svc` + agent card) →
  call over A2A → kill.
- Delegation policy in the parent's template (`allowedChildTypes`,
  `grantableScopes`, `maxDepth`).
- **Least-privilege attenuation across the boundary** — the child receives, and
  can only use, exactly the authority the parent was permitted to grant.
- That **delegation is additive**: the same handoff without it is the
  own-authority variant above, with the child's permissions defined entirely on
  its own type.
