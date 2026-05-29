# Agent Delegation via OAuth2 Token Exchange (RFC 8693)

> Status: **design** (pre-implementation). This note is the conceptual basis for
> the implementation plan. It records the decisions we've locked, the trust
> model, and the known v1 simplifications.

## Goal

Let a parent agent delegate authority to a child agent (and the child to its own
children, forming an arbitrarily long chain) such that:

1. **No inheritance.** A child does not automatically receive its parent's
   permissions. Each hop grants only what policy explicitly allows, and can only
   *narrow* (least privilege down the chain).
2. **The token carries the delegation chain.** Every downstream token names the
   full lineage via the RFC 8693 `act` (actor) claim, so any resource server or
   auditor can see exactly who is acting on behalf of whom.
3. **Any link is revocable/pausable.** If an agent in the chain goes rogue, an
   operator can suspend it and have that take effect at **both** the
   authentication layer (IdentityServer stops issuing/refreshing) and the
   authorization layer (SpiceDB denies), with the rogue agent's **entire
   subtree** revoked as a consequence.
4. **Delegation is governed by policy.** Which parent agent-types may delegate to
   which child-types — plus scope ceilings and max chain depth — is configured in
   the registry and enforced centrally.

## Two identities, mapped onto RFC 8693 roles

The platform already issues the two tokens RFC 8693 needs:

- **SPIRE JWT-SVID** — the agent's cryptographic *workload identity*
  ("I am `agent:child`"). Short-lived, non-transferable, anchored in SPIRE.
  → Used as the **`actor_token`** in an exchange: it proves the new delegate's
  identity.
- **IdentityServer access token** — the *delegated capability* carrying `sub`,
  the `act` chain, and `scope`.
  → Used as the **`subject_token`** that flows down the chain.

Today the sidecar performs `grant_type=client_credentials` with the SVID as
`client_assertion` and receives a flat token (`cmd/agent-sidecar/main.go`,
`tokenCache.get`). Delegation adds a second mode:
`grant_type=urn:ietf:params:oauth:grant-type:token-exchange`.

We use RFC 8693 **delegation** semantics (actor remains visible in `act`), **not
impersonation** (actor erased).

## The delegation hop

When `agent:child` needs to act, its sidecar calls IdentityServer with:

- `actor_token` = child's **SVID** (proves the child is the new actor),
- `subject_token` = the **parent's access token** (the authority being delegated),
- `requested_scope` ⊆ what policy allows.

IdentityServer validates everything (below) and mints the child's token, building
the `act` chain. Per RFC 8693 §4.1, the **outermost `act` is the most recent
actor**; earlier actors nest inside:

```
parent token:   { sub: user-1, act: { sub: agent:parent } }
child token:    { sub: user-1, act: { sub: agent:child,      act: { sub: agent:parent } } }
grandchild:     { sub: user-1, act: { sub: agent:grandchild, act: { sub: agent:child, act: { sub: agent:parent } } } }
```

Each exchange **wraps the new actor around the previous `act`**, and `sub` (the
human user) is carried unchanged down the whole chain.

**Where it runs:** the sidecar. Its `/token` endpoint gains an optional
`subject_token` parameter — if present it performs a token-exchange (actor =
this agent's SVID, subject = the passed token); if absent it falls back to
today's `client_credentials`. The agent's own code stays delegation-blind, in
keeping with the platform's dependency-direction rule (framework/concern glue
lives in the per-agent sidecar/SDK, never leaking into the agent or the platform
core).

## Subject = the human user (`sub: user-1`)

`sub` is the originating human user, carried unchanged through every hop, so a
downstream call always answers "on whose behalf?" with the user, while `act`
answers "through which agents?".

**v1 simplification (locked):** `userId` is currently an unauthenticated field on
the spawn request. We **trust the orchestrator** — already the privileged spawner —
to assert the user when the top-level agent's first token is minted. A real
user-authentication story (the user presenting a verifiable token that seeds the
chain) is **deferred**; see [Deferred](#deferred--revisit-later).

## Token delivery: per-invocation

The child is a long-lived A2A service that different parents may call, so
delegation travels **with the request**, not baked in at spawn:

- The parent includes its access token when it invokes the child over A2A
  (a delegation/`Authorization` header on the A2A call).
- The child's sidecar uses that token as the `subject_token` when it needs a
  downstream token.

This scopes delegation to the actual invocation and preserves child reuse across
tenants. To bound the blast radius of passing a bearer token to the child, the
parent's token should be **short-lived and audience-restricted** (audience = the
child).

## No inheritance — explicit attenuation

The child's granted scopes are:

```
granted = requested ∩ (parent's scopes) ∩ (child template's grantable ceiling)
```

A child can never exceed its parent, never exceeds its own type's ceiling, and
receives nothing implicitly. The parent's scopes are read from the
`subject_token`'s `scope` claim (trusted because IdentityServer signed it).
Scopes should narrow at each hop.

## Delegation policy (registry-owned)

Delegation policy is configured on the agent template in the registry
(`internal/registry/types.go`) and enforced at IdentityServer during the
exchange. Shape (illustrative):

```jsonc
"delegation": {
  "allowedChildTypes": ["child-agent", "research-agent"],
  "grantableScopes":   ["sample-api:read"],   // ceiling this type may pass down
  "maxDepth":          3
}
```

At exchange time IdentityServer asks the registry:

- May `parentType` delegate to `childType`?
- Is the resulting chain within `maxDepth`?
- Is each requested scope within the parent type's `grantableScopes`?

…and rejects the exchange otherwise. The registry already tracks `parentId` and
`agentType`, so it can reconstruct/validate the chain and enforce depth. All
delegation policy lives in the registry to keep it the central source of truth
for agent configuration.

## Revocation / pause

**Single source of truth:** an agent's status in the registry gains
`suspended`/`revoked` (alongside today's `active`/`completed`/`failed`). Pausing a
rogue agent is one state change.

Because every descendant's token contains the rogue agent in its `act` chain, a
single suspension can deny the agent **and its entire subtree**. Enforcement is
layered:

- **Stop new authority (instant, at IdentityServer).** Every exchange checks the
  full `act` chain of the `subject_token` against the registry's suspension
  state. A suspended agent — or any descendant whose chain contains it — is
  refused a fresh token and cannot delegate further. No window.
- **Invalidate in-flight tokens.** Two mechanisms, used together:
  - **(A) Short TTLs + re-exchange — universal backstop.** Delegated tokens are
    short-lived; an already-issued token works until it expires, then re-exchange
    is refused. Bounds the damage window to ≤ one TTL everywhere, including paths
    that don't reach the authz layer.
  - **(C) SpiceDB ancestor-suspension check — instant on protected calls.**
    Resource servers already perform a SpiceDB permission check. Suspension is
    modeled in SpiceDB so that existing check denies the moment suspension is
    written — instant cascade revocation, no extra round-trip.

We deliberately **do not** use active token introspection (RFC 7662) per request:
(C) already gives instant revocation on the authz-critical paths, and (A) covers
the rest.

### Revocation timing summary

| Concern | Mechanism | Latency |
|---|---|---|
| Rogue agent obtaining new / refreshed tokens | IS chain check at exchange | Instant |
| Rogue agent delegating further | IS chain check at exchange | Instant |
| In-flight token on a protected (SpiceDB-checked) action | SpiceDB ancestor-suspension (C) | Instant (next call) |
| In-flight token on any other path | Short TTL (A) | ≤ one TTL |

## Where the delegation graph lives: registry **and** SpiceDB

Two distinct concerns, each in the store that fits it:

- **Registry = system of record.** Owns delegation policy, the chain (`parentId`),
  and suspension state. IdentityServer reads it at exchange time for policy,
  attenuation, and chain-revocation. Keeps the registry the central source of
  truth for agent config.
- **SpiceDB = derived enforcement projection.** The registry writes the
  delegation relationships and suspension flags into SpiceDB — extending the
  existing pattern where the registry projects SpiceDB tuples on registration
  (`cmd/registry/main.go`) and clears them via `DeleteAgentRelationships`.
  Resource servers check SpiceDB for fast per-request AuthZ and cascade
  revocation (mechanism C).

**Consistency:** on suspend, the registry writes to SpiceDB **synchronously**
before acking, so the enforcement projection can't lag the source of truth.
SpiceDB is authoritative for the per-request enforcement check; the registry is
authoritative for config and issuance. The short-TTL backstop (A) also covers any
transient projection lag. This is the same write-through coupling already
accepted when the registry projects relations on registration — more of it, not a
new class of risk.

## Trust topology (who owns what)

| Component | Role in delegation |
|---|---|
| **SPIRE** | Root of *workload* identity; issues the SVIDs that prove each new actor (`actor_token`). |
| **IdentityServer** | Delegation authority; performs the token-exchange, builds the `act` chain, enforces policy + attenuation + chain-revocation at issuance. |
| **Registry** | System of record: delegation policy (allowed edges, grantable scopes, max depth), the chain, and suspension state. |
| **SpiceDB** | Fine-grained AuthZ + the delegation graph projection for per-request cascade-revocation checks. |
| **Orchestrator** | Privileged spawner; trusted (v1) to assert the human `userId` that seeds the chain's `sub`. |
| **Sidecar** | Per-agent policy-enforcement point; performs the exchange (`subject_token` → delegated token). |

## Locked decisions

1. **Root subject** = the human user (`sub: userId`), carried unchanged down the chain.
2. **Delivery** = per-invocation; parent passes a short-lived, audience-restricted token on the A2A call; child's sidecar exchanges it.
3. **Attenuation** = `requested ∩ parent ∩ child-template ceiling`; **scope ceilings + maxDepth** enforced at IS via registry policy.
4. **`act` chain** = full lineage in every token (RFC 8693 delegation semantics).
5. **Revocation** = registry is source of truth; instant at issuance (IS chain check) + instant on protected calls (SpiceDB ancestor check) + short-TTL backstop.
6. **Graph** = registry (truth) projected into SpiceDB (enforcement); all policy registry-side.
7. **Cascade depth** = bounded by `maxDepth` (no unbounded ancestor walk).

## Deferred / revisit later

- **Real user authentication.** v1 trusts the orchestrator to assert `userId`.
  Later: the user presents a verifiable identity/token that seeds the chain's
  `sub`, removing the orchestrator-trust assumption.
- **`may_act` in-token policy (RFC 8693 §4.4).** Keeping all delegation policy in
  the registry for now (central source of truth). Revisit `may_act` once we
  decide how it would be *configured* — it must not pull policy truth out of the
  registry.
- **Token introspection (RFC 7662)** per request — not needed given (A)+(C);
  revisit only if a protected path can't be covered by a SpiceDB check.

## Suggested implementation milestones (for the later plan)

1. **Exchange + `act` chain + attenuation.** Sidecar `/token` gains
   `subject_token` mode; IS supports the token-exchange grant, builds the chain,
   enforces scope attenuation; parent passes its token on the A2A call.
2. **Registry delegation policy.** `delegation` block on templates
   (`allowedChildTypes`, `grantableScopes`, `maxDepth`); IS enforces it at exchange.
3. **Suspension / revocation across both layers.** `suspended` status in the
   registry; IS chain-revocation at issuance; registry → SpiceDB projection of
   delegation + suspension; resource-server cascade check; short TTLs.
