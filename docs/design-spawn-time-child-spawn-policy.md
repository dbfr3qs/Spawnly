---
title: "Design note: spawn-time child-spawn policy"
description: Governing the parent→child spawn edge (allowedChildTypes) independently of scope delegation — deny-by-default at spawn.
---

# Design note: spawn-time child-spawn policy

> Status: **implemented** (edge check). Companion to
> [delegation-design.md](delegation-design.md). Captures a policy gap surfaced
> while documenting [own-authority handoffs](authoring/03-parent-and-child.md#variant-handing-off-without-delegation-own-authority-child),
> now closed for the parent→child edge. `maxDepth`-at-spawn and the
> `spawnableChildTypes` naming split remain deferred (see
> [Out of scope](#not-doing-and-why)).
>
> **What shipped:** registry endpoint `GET /v1/spawn-policy?parentId=&childType=`
> returns `{allowed, reason}` by resolving `parentId → agentType → template` and
> checking the parent template's `allowedChildTypes` (deny-by-default). The
> orchestrator calls it in `POST /spawn` whenever a `parentId` is present and
> returns `403` on denial (emitting a `spawn_denied` event on the parent's
> stream). See `cmd/registry/main.go`, `internal/registry/client.go`, and
> `cmd/orchestrator/main.go`.

## The gap

A parent agent spawning a child is governed by the `delegation` block on the
parent's template — `allowedChildTypes`, `grantableScopes`, `maxDepth`. But that
policy is consulted in exactly **one** place: IdentityServer, during a
**token-exchange** ([`TokenExchangeGrantValidator.cs`](../identityserver/TokenExchangeGrantValidator.cs),
via the registry's [`GET /v1/delegation-policy`](../cmd/registry/main.go#L381)).

The spawn path itself enforces none of it. The orchestrator's `POST /spawn`
([`cmd/orchestrator/main.go`](../cmd/orchestrator/main.go#L144)) reads the
template only for `runtimeSpec.lifecycle`, then writes the `AgentWorkload` CRD. It
does not check `allowedChildTypes`, `maxDepth`, or `parentId` against any policy.

**Consequence:** the parent→child *relationship* is only ever validated as a
side effect of scope delegation. In a handoff that uses the child's **own
authority** (no token-exchange — see the
[own-authority variant](authoring/03-parent-and-child.md#variant-handing-off-without-delegation-own-authority-child)),
no exchange happens, so **any parent may spawn and call any child type,
ungoverned**. The "may call" edge and the "may delegate scopes" edge are
currently bundled into a single gate, and that gate fires only on the scope path.

## Why it matters

- **Agnostic, config-defined policy.** The platform's goal is that the enterprise
  defines policy entirely via configuration. "Which agents may hand off to which"
  is a policy question that today has no enforcement surface unless delegation is
  also in play.
- **Two genuinely separate concerns.** "Parent P may invoke child C" (a topology
  / relationship decision) is distinct from "P may delegate scope S to C" (an
  authority-flow decision). Bundling them means you cannot govern handoffs
  without also flowing authority — and cannot adopt the safer own-authority model
  while keeping the relationship governed.

## Proposed direction

**Split the edge from the attenuation.** Enforce the relationship at spawn;
keep scope attenuation at exchange.

1. **Spawn-time check (new).** When `POST /spawn` carries a `parentId`, the
   orchestrator (or registry, as system of record) resolves the parent's
   `agentType`, looks up the parent template's `delegation.allowedChildTypes`, and
   rejects the spawn if the requested child type isn't listed. Reuse the existing
   [`GET /v1/delegation-policy`](../cmd/registry/main.go#L381) decision — it
   already answers "may parentType delegate to childType?"; here it answers "may
   parentType spawn childType?".
2. **Depth at spawn (optional).** `maxDepth` can also be checked here against the
   registry chain (`parentId` lineage), bounding spawn fan-out independently of
   token depth.
3. **Exchange-time check (unchanged).** `grantableScopes` and the
   `requested ⊆ subject` attenuation stay where they are — they're about authority
   flow, which only the exchange path exercises.

### Naming consideration

If the edge is enforced independently of delegation, `allowedChildTypes` is
arguably misnamed (it reads as delegation-specific). A clearer split might be a
`spawnableChildTypes` (topology) distinct from the delegation `grantableScopes`
(authority). Decide before implementing, since it touches the template schema in
[`internal/registry/types.go`](../internal/registry/types.go).

## Open questions

- **Orchestrator vs registry** as the enforcement point. The orchestrator owns
  `/spawn`; the registry owns policy. Enforcing in the registry keeps policy
  central (the orchestrator already calls the registry for templates), but adds a
  round-trip on the spawn path.
- **Default posture** for a template with no `delegation` block: deny all child
  spawns (secure default, requires every parent to opt in) or allow (backward
  compatible with today's ungoverned behavior). The agnostic-platform goal argues
  for deny-by-default + explicit config.
- **Self-service spawns.** This only concerns agent-initiated spawns (`parentId`
  set). Operator/dashboard-initiated spawns have no parent and are out of scope.

## Not doing (and why)

- Enforcing the edge by *requiring* a delegation token on every handoff — that
  would force the delegation model onto own-authority handoffs, the opposite of
  the goal.
- Moving scope attenuation to spawn time — attenuation is per-invocation and
  belongs with the exchange; spawn happens once.
