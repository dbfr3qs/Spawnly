---
title: "Implementation Plan: Agent Delegation via Token Exchange"
description: How the delegation milestones (token exchange, registry policy, suspension/revocation) were executed.
---

# Implementation Plan: Agent Delegation via Token Exchange

Companion to [delegation-design.md](delegation-design.md). Executed with sub
agents, phased by the three milestones. Contracts are fixed up front so
workstreams can build in parallel.

## Acceptance test (end state)

1. Spawn **parent-agent** (user-1). Sidecar mints a root token:
   `sub: user:user-1`, `act: {sub: <parent SVID>}`, `aud: sample-api-a`,
   `scope: sample-api-a:read sample-api-a:write`.
2. Parent calls **API-A** `POST /work` (write) → **200**.
3. Parent mints a **delegation token** (`aud: delegation`,
   `scope: sample-api-b:read`) and passes it to the **child** over A2A.
4. Child's sidecar **exchanges** it (actor = child SVID, subject = delegation
   token) → `sub: user:user-1`, `act: {sub: child, act: {sub: parent}}`,
   `aud: sample-api-b`, `scope: sample-api-b:read`.
5. Child calls **API-B** `GET /work` → **200**; `POST /work` → **403**
   (attenuated, no write); child calls **API-A** with that token → **401**
   (wrong audience). The delegation token itself is rejected by **every**
   resource server (`aud: delegation`).
6. **Suspend the parent** → child's next API-B call → **403** (ancestor
   suspended, cascade) and child's next exchange → **rejected** at IS; parent's
   own API-A call → **403/401**.

## Fixed contracts

**Token shape (IS-issued).** Always `sub = user:<userId>`; `act` = nested agent
chain (outermost = current actor, value = SVID URI), earlier actors nested
inside; `aud` = target API (or `delegation`); `scope` = granted. The root
client_credentials token already carries `act: {sub: <agent SVID>}`.

**Reserved `delegation` audience (Option B).** A token a parent hands to a child
is minted with `aud: delegation` and only the scope(s) being delegated. No
resource server accepts `aud: delegation`, so it can't be replayed at any API;
only IS will exchange it. This prevents handing the child a usable parent
credential (upholds no-inheritance).

**Exchange request (sidecar → IS):**
`grant_type=urn:ietf:params:oauth:grant-type:token-exchange`,
`subject_token=<delegation token>`,
`subject_token_type=urn:ietf:params:oauth:token-type:access_token`,
`actor_token=<child SVID>`, `actor_token_type=urn:ietf:params:oauth:token-type:jwt`,
`audience=<target API>`, `scope=<requested>`; client auth via existing
`client_assertion=<SVID>`.

**Sidecar `/token`** gains `subject_token` + `audience` params: present ⇒
token-exchange; absent ⇒ today's client_credentials.

**IS clients (hardcoded in `Config.cs` for now; registry-driven deferred).**
Each authenticated by SVID (placeholder secret), granted `client_credentials`
**and** token-exchange:

| client_id (= agent type) | AllowedScopes |
|---|---|
| `parent-agent` | `sample-api-a:read`, `sample-api-a:write`, `sample-api-b:read` |
| `child-agent`  | `sample-api-b:read` |

(`worker`, `weather-monitor` unchanged.)

**Scopes / audiences.** `sample-api-a:{read,write}` (aud `sample-api-a`),
`sample-api-b:{read,write}` (aud `sample-api-b`), plus reserved `delegation`
audience. `GET → :read`, `POST → :write`.

**Resource server = one parameterized `sample-api` binary**, deployed twice
(`sample-api-a`, `sample-api-b`) via env `API_AUDIENCE`/`SCOPE_PREFIX`. It
validates issuer **and** audience (== its own; rejects `delegation`), maps
method→scope, extracts the **acting agent** (outermost `act.sub`) and the
**full chain**, authorizes the acting agent (`work_on`), and (M3) denies if any
chain member is suspended.

**Registry additions.** Template: `delegation {allowedChildTypes[],
grantableScopes[], maxDepth}`, `allowedScopes[]`, `allowedAudiences[]`.
`AgentRecord.Status` gains `suspended`. Endpoints:
`GET /v1/agents/{id}/chain` (lineage + each link's status),
`GET /v1/delegation-policy?parentType&childType`,
`POST /v1/agents/{id}/suspend|resume`.

**SpiceDB.** Per-agent `suspended` marker + `active` permission; registry
projects suspend/resume synchronously (extends the existing tuple projection).

**Root `sub: user` (v1 simplification):** IS reads `userId` from the registry
agent record (set by the trusted orchestrator at spawn) and stamps
`sub: user:<userId>`.

## Milestone 1 — Token exchange + act chain + audience/scope attenuation

- **SA-1 · IdentityServer (C#).** Token-exchange `IExtensionGrantValidator`;
  add the two clients + scopes/audiences + reserved `delegation` audience to
  `Config.cs`; `AgentRegistryValidator` sets `sub: user:<userId>` (+ `act` on
  root); exchange validates subject+actor (SVID), carries `sub`, nests `act`,
  sets `aud`, grants `scope = requested ∩ subject.scope` (registry ceiling is
  M2); `AgentRegistryClient.GetAgent` for userId. *Tricky bit:* forcing
  `aud: delegation` on the delegation token (resource indicator / custom token
  request) — fallback is a `token_use: delegation` claim that resource servers
  reject.
- **SA-2 · Go platform + deploy.** Sidecar `/token` exchange mode;
  `tokenvalidator` returns `{user, actingAgent, chain[], scopes[]}` and
  validates audience; parameterize `sample-api` (`API_AUDIENCE`/`SCOPE_PREFIX`,
  method→scope, `work_on` on acting agent); second `sample-api` Deployment/
  Service manifests; operator passes `API_A_URL`/`API_B_URL`.
- **SA-3 · TS agents + seed.** Parent calls API-A, mints delegation token
  (`aud: delegation`, `scope: sample-api-b:read`), passes it on the A2A call;
  child exchanges via sidecar and calls API-B; bootstrap/reseed register the
  two sample-api URLs.

*Order:* SA-1 owns the token contract; SA-2 builds to it (parses standard
claims); SA-3 integrates last (after the real token shape is verified on the
cluster). **I integrate + verify acceptance steps 1–5.**

## Milestone 2 — Registry-driven delegation policy

- **SA-4 · Registry (Go).** `delegation`/`allowedScopes`/`allowedAudiences` on
  templates; `delegation-policy` + `chain` endpoints; lineage from `parentId`;
  tests.
- **SA-5 · IdentityServer (C#).** At exchange: enforce `allowedChildTypes`
  (parent→child edge), `grantableScopes` ceiling
  (`granted ⊆ requested ∩ subject.scope ∩ ceiling`), and `maxDepth`; reject with
  `invalid_grant` + reason.

*Order:* SA-4 then SA-5. **Verify:** disallowed child-type / over-ceiling scope /
over-depth all rejected.

## Milestone 3 — Suspension + cascade revocation

- **SA-6 · Registry + SpiceDB.** `suspended` status + `suspend`/`resume`
  endpoints; `IsActive`/policy treat suspended as inactive; `schema.zed`
  suspension marker + `active` permission; synchronous projection into SpiceDB;
  short delegated-token TTLs; tests.
- **SA-7 · IdentityServer (C#).** At exchange, walk the subject_token `act`
  chain (via registry `chain`) and reject if any member is suspended.
- **SA-8 · Resource server (Go).** On each call, check every chain member's
  non-suspension via SpiceDB (bounded by `maxDepth`); deny on any suspended
  ancestor.

*Order:* SA-6 first, then SA-7 ⟂ SA-8. **Verify:** full acceptance test incl.
step 6.

## Cross-cutting

- **Backward compatibility:** the non-delegated client_credentials + single
  `work_on` path keeps working (worker/weather-monitor unaffected).
- **Sub-agent execution:** C# (`identityserver/`) and pure-Go workstreams in
  isolated worktrees; TS/SDK (`agents/*`, gitignored `node_modules`/`dist`) in
  the main tree. Disjoint file sets per agent.
- **Verification:** per milestone on the kind cluster (spawn-parent +
  port-forward); the `act` chain and 401/403s show up in the observability
  event/log timeline.
- **Commits:** one commit per milestone on `feat/delegation`.

## Deferred / revisit

- **Registry-driven IS `IClientStore`** (clients sourced from templates instead
  of `Config.cs`).
- Real user authentication (replaces orchestrator-trusted `userId`).
- `may_act` in-token policy; RFC 7662 introspection.
