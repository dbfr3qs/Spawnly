---
title: Defining Agent Policy
description: An agent's own authority (authzTemplate + scopes) and parent→child delegation (the delegation block), with a set-vs-consume component map.
---

# Defining Agent Policy

> **Prerequisite:** [04 — Defining a Template](04-defining-a-template.md). Policy
> lives in two template blocks: `authzTemplate` and `delegation`.
>
> **Conceptual background:** [delegation-design.md](../delegation-design.md) is
> the design note — the *why*, the trust model, and the locked/deferred
> decisions. This guide is the operational *how*: what to write and what each
> component does with it. Read the design note for rationale; read this to ship.

There are **two distinct policy systems** on the platform, and the first job of
this guide is to keep them separate in your head:

1. **An agent's own authority** — what a single agent may do, via
   `authzTemplate` (SpiceDB relations) + the scopes it requests. → [Part 1](#part-1--an-agents-own-authority).
2. **Delegation policy** — what a *parent* may hand to a *child*, via the
   `delegation` block, enforced at token-exchange. → [Part 2](#part-2--delegation).

---

## How policy is set and consumed

Policy is **authored** in templates (one place) but **consumed** at several
points and moments. This map is the orientation for everything below:

```
  SET POLICY (author-time)                       CONSUME POLICY (run-time)
  ────────────────────────                       ─────────────────────────

  Template author
       │ POST /v1/templates
       │   { authzTemplate, delegation{} }
       ▼
  ┌─────────────┐  system of record (in-memory)
  │  Registry   │◀──────────────────────────────┐ GET /v1/delegation-policy
  │  templates  │                                │ GET /v1/agents/{id}/chain
  │  + records  │                                │
  │  + suspend  │──┐                             │
  └─────────────┘  │ on self-register: project   │
                   │ authzTemplate relations;    │
                   │ on suspend: drop relations  │
                   ▼                             │
  ┌─────────────┐                       ┌────────┴─────────┐
  │   SpiceDB   │                       │  IdentityServer  │
  │  relation   │                       │  token-exchange: │
  │  tuples     │                       │   allowedChild   │
  └──────┬──────┘                       │   grantable (∩)  │
         │ per-request check            │   maxDepth       │
         │                              │   act-chain +    │
         ▼                              │   suspension     │
  ┌─────────────┐   Bearer token        └────────┬─────────┘
  │  Sample API │◀───────────────┐    issues attenuated token
  │ (resource   │                │       (scope ⊆ ceiling, act)
  │  server):   │           ┌────┴─────┐          │
  │  work_on    │           │  Sidecar │◀─────────┘
  │  on EVERY   │           │ /token   │  mint (audience=delegation)
  │  chain hop  │           │ mint /   │  exchange (subject_token=…)
  └──────┬──────┘           │ exchange │
         ▲                  └────┬─────┘
         │ Bearer                │ /token?scope | subject_token
         └──────────┬────────────┘
                    │
              ┌─────┴─────┐
              │   Agent   │
              └───────────┘
```

| Policy element | Set by | Stored in | Consumed by | When |
|----------------|--------|-----------|-------------|------|
| `authzTemplate.spiceDbRelations` | template author | Registry → SpiceDB | SpiceDB tuples; Sample API (`work_on`) | written at self-register; checked per protected call |
| requested `scope` | agent code | (carried in token) | IdentityServer (issue); Sample API (validate) | per token request / per call |
| `delegation.allowedChildTypes` | parent template | Registry | IdentityServer via `/v1/delegation-policy` | at token-exchange |
| `delegation.grantableScopes` | parent template | Registry | IdentityServer (attenuation ∩) | at token-exchange |
| `delegation.maxDepth` | parent template | Registry (+ chain) | IdentityServer | at token-exchange |
| suspension state | operator (`/suspend`) | Registry → SpiceDB | IS (chain check); SpiceDB (per-call) | at exchange + per protected call |

---

## Part 1 — An agent's own authority

### Tutorial: authorise an agent to call a protected API

**Goal:** let a `report-builder` agent read `sample-api-a`.

**Step 1 — grant the relationship.** In the template's `authzTemplate`, declare
the relation the agent needs. The standard tenant-membership grant is:

```json
"authzTemplate": {
  "spiceDbRelations": [
    { "resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}" }
  ]
}
```

When an agent of this type self-registers, the registry expands the tokens and
writes `tenant:tenant-1#agent@agent:report-builder-xxxxx` into SpiceDB. That
tuple satisfies the `work_on` permission in the schema
([`schema.zed`](../../deploy/spicedb/schema.zed)):

```
definition tenant {
    relation agent: agent
    permission work_on = agent   // any related agent may work_on the tenant
}
```

**Step 2 — request a scoped token.** The agent asks its sidecar (via the SDK's
`TokenClient`) for a token carrying the scope the API requires:

```ts
import { TokenClient } from '@spawnly/sdk';

const tokens = new TokenClient();
const accessToken = await tokens.getToken('sample-api-a:read');
```

**Step 3 — call the API.** Send the Bearer token and the tenant header:

```ts
await fetch(`${process.env.API_A_URL}/work`, {
  method: 'GET',
  headers: { Authorization: `Bearer ${accessToken}`, 'X-Tenant-ID': tenantId },
});
```

### What the resource server checks

`sample-api`'s `authorize()` ([`cmd/sample-api/main.go`](../../cmd/sample-api/main.go))
runs these gates, in order:

1. **`X-Tenant-ID` present** — which tenant is this call for? *(tenant gate — see below)*
2. **Scope present** — the token must carry the scope for the method
   (`:read` for GET, `:write` for POST). Missing scope → `403`.
3. **`work_on` on every chain hop** — for the calling agent *and every actor in
   its `act` chain*, SpiceDB must say `tenant:T#work_on@agent:<id>`
   ([main.go:78](../../cmd/sample-api/main.go#L78)). Any member lacking it → `403`. *(tenant gate)*

Gate 3 is what makes suspension cascade (Part 2): drop one agent's relation and
every protected call whose chain includes it is denied.

The two **tenant gates** (1 and 3) are active only when the instance requires a
tenant (`REQUIRE_TENANT`, default `true`). A
[tenant-agnostic instance](#tenant-agnostic-resource-servers) skips both,
validating only the token signature, audience, and scope — that's what lets a
[global agent](04-defining-a-template.md#tenanted-vs-global-agents) call it.

---

## Part 2 — Delegation

### Tutorial: let a parent delegate read-only access to a child

**Goal:** parent `report-builder` lets child `data-fetcher` **read**
`sample-api-b`, but never write it — even though nothing about the child's own
identity grants it that access. The authority is *delegated and attenuated*.

**Step 1 — declare the delegation ceiling on the parent's template.** This is the
policy gate; without it, no exchange across this edge is allowed:

```json
"delegation": {
  "allowedChildTypes": ["data-fetcher"],
  "grantableScopes":   ["sample-api-b:read"],
  "maxDepth":          3
}
```

**Step 2 — the parent mints a delegation token.** Its sidecar issues a token with
the sentinel `audience=delegation` (not usable at any resource server, only
re-exchangeable at IS) and the scope it intends to pass down:

```ts
// parent: client-credentials with audience=delegation
// (wire: GET /token?audience=delegation&scope=sample-api-b:read)
const delegationToken = await tokens.getToken('sample-api-b:read', { audience: 'delegation' });
```

**Step 3 — the parent hands the token to the child** over A2A (message metadata),
and the child's sidecar **exchanges** it — actor = the child's SVID, subject =
the delegation token:

```ts
// child: RFC 8693 token-exchange
// (wire: GET /token?subject_token=<delegationToken>&audience=sample-api-b&scope=sample-api-b:read)
const exchanged = await tokens.exchangeToken({
  subjectToken: delegationToken,
  audience: 'sample-api-b',
  scope: 'sample-api-b:read',
});
```

**Step 4 — observe the attenuation.** With the exchanged token the child reads
`sample-api-b` successfully (`200`) but a write is denied (`403`) — the scope was
never granted. That `403` is the visible proof least privilege held across the
boundary.

This is exactly the [Scenario 3](03-parent-and-child.md) flow; the
`parent-agent`/`child-agent` reference code implements it.

### What IdentityServer enforces at the exchange

[`TokenExchangeGrantValidator`](../../identityserver/TokenExchangeGrantValidator.cs)
rejects the exchange unless **all** of these hold:

1. **Valid actor.** `actor_token` is a valid SPIRE SVID; its SPIFFE id is the new
   actor (the child).
2. **Scope ⊆ parent.** Every requested scope is present in the `subject_token`'s
   `scope` (the parent's actual authority).
3. **Edge allowed.** Registry `/v1/delegation-policy?parentType&childType` returns
   `allowed: true` (from the parent template's `allowedChildTypes`).
4. **Scope ⊆ ceiling.** Every requested scope is in the parent type's
   `grantableScopes`.
5. **Depth.** `subject act-chain depth + 1 ≤ maxDepth`.
6. **Whole chain active.** The child *and every actor named in the subject's `act`
   chain* must be `active` in the registry — any `suspended`/`failed`/`completed`
   member rejects the exchange.

Gates 2 and 4 together are the attenuation rule:

```
granted = requested ∩ parent-scopes ∩ child-type-ceiling
```

On success IS wraps the new actor around the existing chain:
`act = { sub: <child spiffe>, act: <subject_token.act> }`.

### Why delegation only narrows

A recurring instinct is: "if policy allows the parent→child edge, why not let the
exchange mint *completely different* scopes the parent never held? The token would
just record that the parent handed off to the child." It's worth being precise
about why the platform refuses this.

- **A token is an authorization assertion, not a call-graph record.** With `sub`
  fixed to the user down the whole chain, a downstream token *asserts* "this is
  user-1's authority, wielded by `agent:child` via `agent:parent`." If the child
  minted a scope neither the user nor the parent held, that assertion is false —
  you wouldn't be recording a handoff, you'd be **forging an authorization**. A
  resource server can't tell the difference, and shouldn't have to: a constant
  `sub` is exactly what lets it trust "scopes here ⊆ what the subject authorized."
- **"Different scopes, on whose behalf?" has no coherent answer.** If the child
  exercises authority *as the user*, the user must have held it. If it exercises
  authority *as itself*, then `sub` should be the child — which is the
  [own-authority handoff](03-parent-and-child.md#variant-handing-off-without-delegation-own-authority-child),
  not delegation.
- **Expansion is the confused-deputy pattern.** Letting a callable child mint
  authority for whoever invoked it turns "P may call C" into "P may wield C's
  powers" — silent escalation via composition, and an audit story where "what can
  P do?" becomes the transitive closure over every reachable child.
- **The lineage you want is already captured elsewhere.** `parentId` /
  [`/v1/agents/{id}/chain`](../../cmd/registry/main.go#L414) and the event log
  record the handoff regardless of scope, so expansion buys no provenance the
  platform doesn't already have — it only weakens the authority invariant.
- **In this codebase, attenuation is load-bearing.** The resource server gates on
  the scope string + per-tenant `work_on`, with **no per-scope SpiceDB check**, so
  the IS `requested ⊆ subject` gate is the *only* thing stopping expansion from
  granting access. Relax it and `grantableScopes` flips from "what P may share" to
  "what P may summon that it never had."

The takeaway: model *different* authority as the child's **own** authority (its
template + own ceiling) and reserve delegation for *narrowing* a slice of the
user's authority. Keep using token-exchange for the `act` chain it produces — just
bound the result by the subject. Full design rationale:
[delegation-design.md](../delegation-design.md#no-inheritance--explicit-attenuation).

### Revocation / suspension

Suspending one agent revokes it **and its entire subtree**, enforced in three
layers (see [delegation-design.md](../delegation-design.md#revocation--pause) for
the rationale):

| Concern | Mechanism | Latency |
|---------|-----------|---------|
| New / refreshed tokens through a suspended agent | IS chain-active check (gate 6) | Instant |
| In-flight token on a protected call | SpiceDB drop on suspend → resource-server `work_on` fails on that hop | Instant (next call) |
| In-flight token on any other path | Short token TTL (120s) | ≤ one TTL |

Operations:

```bash
curl -sf -X POST http://localhost:8080/v1/agents/<id>/suspend   # status=suspended, drop SpiceDB relations
curl -sf -X POST http://localhost:8080/v1/agents/<id>/resume    # re-derive relations from template, status=active
```

`suspend` sets the record to `suspended` and calls `DeleteAgentRelationships`;
`resume` re-writes the template's relations and restores `active`
([cmd/registry/main.go:326](../../cmd/registry/main.go#L326)).

---

## Reference appendix

### SpiceDB model and extending it

Today's schema ([`schema.zed`](../../deploy/spicedb/schema.zed)) is deliberately
minimal:

```
definition agent {}
definition tenant {
    relation agent: agent
    permission work_on = agent
}
```

To authorise a **new resource**, add a definition with its own relations and
permissions, then have templates write the matching relations via
`authzTemplate.spiceDbRelations`. The resource server for that resource then
calls `CheckPermission(resource, permission, agent:<id>)`. Keep the projection
pattern: the registry writes tuples; resource servers only read.

### Tenant-agnostic resource servers

Tenant enforcement is a **per-resource-server choice**, not a platform-wide
rule. A `sample-api` instance reads `REQUIRE_TENANT` (default `true`):

| `REQUIRE_TENANT` | Behaviour |
|------------------|-----------|
| `true` (default) | Demands `X-Tenant-ID` and checks `work_on` on every chain hop (gates 1 + 3 above). |
| `false` | **Tenant-agnostic:** skips both tenant gates; still validates token signature, `token_use`, audience, and scope. |

A tenant-agnostic instance is what a
[global agent](04-defining-a-template.md#tenanted-vs-global-agents) calls — it
asserts no tenant and holds no `tenant:` grant, so a tenant-checking instance
would (correctly) deny it. The `sample-api-global` manifest
([`deploy/manifests/sample-api-global.yaml`](../../deploy/manifests/sample-api-global.yaml))
and the `global-worker` template
([`agents/global-worker/template.json`](../../agents/global-worker/template.json))
are a worked example of the pair. Setting `REQUIRE_TENANT=false` relaxes **only**
the tenant check — authn and scope are still enforced.

### Scopes and audiences catalog

Defined in IdentityServer ([`Config.cs`](../../identityserver/Config.cs)):

| Scope | Grants |
|-------|--------|
| `sample-api-a:read` / `sample-api-a:write` | read / write `sample-api-a` |
| `sample-api-b:read` / `sample-api-b:write` | read / write `sample-api-b` |
| `sample-api` | legacy, backward-compat |

| Audience (`aud`) | Meaning |
|------------------|---------|
| `sample-api-a` | token usable at sample-api-a (scopes `sample-api-a:*`) |
| `sample-api-b` | token usable at sample-api-b (scopes `sample-api-b:*`) |
| `delegation` | sentinel — **no** resource audience; only re-exchangeable at IS |

Access tokens are short-lived (**120s**, `AccessTokenLifetime`), which is the
TTL backstop in the revocation table.

### Delegation enforcement points

| Field / state | Enforced at | Endpoint / check |
|---------------|-------------|------------------|
| `allowedChildTypes` (spawn edge) | Orchestrator (at `/spawn`, when `parentId` set) | `GET /v1/spawn-policy` → `allowed` (deny-by-default) |
| `allowedChildTypes` (scope delegation) | IdentityServer | `GET /v1/delegation-policy` → `allowed` |
| `grantableScopes` | IdentityServer | requested ⊆ ceiling |
| `maxDepth` | IdentityServer | chain depth + 1 ≤ max |
| `act` chain lineage | Registry | `GET /v1/agents/{id}/chain` |
| suspension | IS + SpiceDB | chain-active check + dropped relations |

### Token modes at the sidecar `/token`

[`cmd/agent-sidecar/main.go`](../../cmd/agent-sidecar/main.go) picks a mode from
the query params; the SDK's `TokenClient` is the typed wrapper over each:

| Params | Mode | SDK call | Use |
|--------|------|----------|-----|
| `scope` only | `client_credentials` (cached) | `getToken(scope)` | an agent's own protected calls |
| `audience` (+ `scope`) | `client_credentials`, explicit audience | `getToken(scope, { audience })` | parent mints a delegation token (`audience=delegation`) |
| `subject_token` (+ `audience`, `scope`) | `token-exchange` | `exchangeToken({ subjectToken, audience, scope })` | child exchanges a delegated token, extending the `act` chain |

### Status callouts

- ⚠️ Suspension/revocation reflects the **M3** implementation; the broader model
  (and deferred items like real user authentication and `may_act`) is described
  in [delegation-design.md](../delegation-design.md#deferred--revisit-later).
- ⚠️ Like all registry state, suspension is **in-memory** — a registry restart
  resets agent records (templates must be re-seeded; see
  [04 — status callouts](04-defining-a-template.md#status-callouts)).
- ⚠️ The `userId` that seeds a chain's `sub` is currently **trusted from the
  orchestrator**, not independently authenticated (v1 simplification).

---

That completes the authoring series: [anatomy](00-anatomy.md) → the three
scenarios → [templates](04-defining-a-template.md) → policy.
