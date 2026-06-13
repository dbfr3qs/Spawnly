# Phase 4 — Persistence: Store interface (+ DynamoDB reference design)

## Goal & why it matters

The registry (`cmd/registry/main.go`) currently holds all state — agent
templates, agent records, event logs, and consent records — in plain Go maps
guarded by a single `sync.RWMutex` (the `store` struct, `cmd/registry/main.go:22-39`).
That's fine for a single-process demo, but it's a dead end for a "SaaS-ready
reference architecture": state vanishes on restart, and there's no way to run
more than one registry replica.

This phase extracts a `Store` interface so the registry's HTTP handlers stop
depending on the concrete map-based struct. The in-memory implementation
becomes one of (potentially several) `Store` implementations — and remains the
**portable default and the test double** used by `main_test.go`. A second,
durable implementation (DynamoDB) is *designed* in this document but not built;
that's a deliberate split so the interface boundary gets validated by a second
consumer (the spec) before any AWS-specific code lands.

This is the foundation for Phase 5 (consent broker state) and for eventually
running the registry as a real multi-replica service — without that ever
requiring the registry's HTTP/business logic to change again.

## Decision: why not relational, why interface-first, why Dynamo is documented-not-built

**Why not a relational DB.** Every access pattern the registry needs is
key-value or hierarchical lookup, never a join or ad-hoc query:

- Get a template by `agentType` (point lookup).
- Get/update an agent record by `agentID` (point lookup).
- Append/list an agent's events (ordered list, append-only, partitioned by
  `agentID`).
- Find a consent by `(userID, parentType, childType)` (point lookup on a
  composite key).
- List all agents (excluding dismissed) / list a user's consents (bounded
  scans over a small, app-controlled set).
- Walk the agent graph **down** from a node (subtree, for revoke/resume) and
  **up** from a node (depth check, delegation chain) — both are graph
  traversals over a `ParentID` edge, not SQL joins.

There is no cross-entity transaction anywhere in the registry (a spawn writes
an agent record, an event, and some SpiceDB tuples — none of that needs to be
atomic with a second entity's row). A relational schema would buy us nothing
here except an operational dependency (a managed Postgres, migrations,
connection pooling) that every access pattern already routes around.

**Why interface-first.** Defining a `Store` interface lets us:

1. Keep today's in-memory map implementation as the default — zero new infra
   for local dev, CI, and `main_test.go`.
2. Swap in a durable implementation later (DynamoDB, or anything else) without
   touching `buildMux` or any HTTP handler — the interface is the seam.
3. Validate the interface shape *before* committing to one backend's quirks,
   by designing (not yet building) a second implementation against it.

**Why DynamoDB is documented, not built, in this phase.** DynamoDB is a
legitimate, even attractive, choice for an AWS production deployment of this
reference architecture — single-digit-ms point lookups, serverless scaling,
and its access-pattern shape (point lookups + partition queries + adjacency
lists for the graph) maps cleanly onto the registry's needs (see the DynamoDB
section below).

But hard-binding the *reference architecture* to one cloud's proprietary
database contradicts the project's portability thesis — "pluggable couplings
with opinionated defaults," not "you must run on AWS." So Phase 4's scope is:

- Build the `Store` interface (the portable contract).
- Build `inMemoryStore` (the portable default + test double) — ship this now.
- **Specify** a DynamoDB single-table design in enough detail that a future
  phase can implement it as a drop-in `Store` without re-deriving the access
  patterns or rediscovering the eventual-consistency wrinkle described below.

This keeps the reference architecture cloud-neutral today while leaving a
concrete, de-risked path to a production AWS backend.

## Current state (file:line evidence)

All of today's state and access logic lives in `cmd/registry/main.go`:

- **`store` struct** (`cmd/registry/main.go:22-30`): four maps under one
  `sync.RWMutex` —
  - `templates map[string]registry.AgentTemplate` keyed by `agentType`
  - `agents map[string]registry.AgentRecord` keyed by `agentID`
  - `events map[string][]events.Event` keyed by `agentID`
  - `consents map[string]registry.ConsentRecord` keyed by
    `consentKey(userID, parentType, childType)` (`main.go:119-121`)
- **`newStore()`** (`main.go:32-39`): constructs the four empty maps.
- **Template access** — `putTemplate` (`main.go:41-45`), `getTemplate`
  (`main.go:47-52`). Point writes/reads keyed by `agentType`.
- **Agent record access** — `registerAgent` (`main.go:54-58`), `getAgent`
  (`main.go:60-64`). Point writes/reads keyed by `agentID`.
- **Event log** — `appendEvent` (`main.go:66-72`, stamps `ID`/`Timestamp` on
  write), `getEvents` (`main.go:74-81`, returns a defensive copy). Append-only
  list keyed by `agentID`.
- **`listAgents`** (`main.go:83-93`): full scan of `agents`, filtering out
  `Dismissed == true`. Used by `GET /v1/agents`.
- **`dismissAgent`** (`main.go:95-105`): read-modify-write on one agent record,
  sets `Dismissed = true`.
- **`updateAgent`** (`main.go:107-117`): read-modify-write on one agent
  record's `Status`.
- **Consent CRUD** (`main.go:119-195`):
  - `consentKey` (`119-121`) — composite key `userID|parentType|childType`.
  - `upsertConsent` (`126-137`) — replace-on-write keyed by edge; preserves
    `ID` across re-grants by reading the existing record first.
  - `findConsent` (`139-144`) — point lookup by edge key.
  - `listConsents(userID)` (`146-158`) — full scan of `consents`, optionally
    filtered to one user.
  - `revokeConsent(id, userID)` (`160-174`) — scan to find by `ID` (+ optional
    `userID` match), then read-modify-write `Revoked = true`.
  - `consentExpiry` (`176-195`) — pure derivation from a template's
    `ChildPolicies[childType].ConsentTTL`; calls `getTemplate`, not a store
    write — this is policy logic, not a storage primitive.
- **Graph traversal — DOWN** — `subtree(id)` (`main.go:201-227`): full scan of
  `agents` to build an in-memory `ParentID -> []childID` adjacency map, then a
  BFS from `id`. This is the set a cascading revoke/resume operates on
  (`POST /v1/agents/{id}/revoke` at `main.go:551-565`,
  `POST /v1/agents/{id}/resume` at `main.go:573-587`).
- **Graph traversal — UP (depth)** — `depth(id)` (`main.go:232-247`): walks
  `ParentID` pointers to the root, counting hops, with a cycle guard. Used by
  the `GET /v1/spawn-policy` handler (`main.go:630-676`, specifically
  `s.depth(parentID) + 1` at `660`) to enforce `Delegation.MaxDepth`.
- **Graph traversal — UP (chain)** — the `GET /v1/agents/{id}/chain` handler
  (`main.go:747-788`) walks `ParentID` pointers from the requested agent to the
  root via repeated `s.getAgent(...)` calls (capped at 32 hops, cycle-guarded),
  building a `[]chainNode` for the dashboard's lineage view. This traversal is
  *already* expressed purely in terms of `getAgent` — a good model for how
  `subtree`/`depth` should also be decomposed.
- **Templates listing** — `GET /v1/templates` handler (`main.go:790-799`)
  currently reaches into `s.mu`/`s.templates` *directly* (not via a store
  method) to build the list of agent types. This is the one place the HTTP
  layer touches store internals and must gain a proper `Store` method
  (`ListTemplateTypes()` or similar).

## Target design — the `Store` interface

Graph traversal (BFS for `subtree`, the up-walks for `depth` and `chain`)
stays in **application code** (`cmd/registry/main.go`). The store exposes only
primitives — `GetAgent` and a new `ListChildren(parentID)` — and the app loops
over them. This is what makes the DynamoDB design tractable (see below): each
traversal step becomes one `Store` call, which a Dynamo impl turns into one
`GetItem`/`Query`.

Proposed interface (new file `internal/registry/store.go`):

```go
package registry

import (
	"context"

	"github.com/spawnly/platform/internal/events"
)

// Store is the registry's persistence boundary. All methods are safe for
// concurrent use. Implementations: inMemoryStore (default, test double) and,
// per docs/saas/phase-4-persistence-store-interface.md, a future DynamoDB
// implementation for AWS production deployments.
type Store interface {
	// Templates — point access keyed by agentType.
	PutTemplate(ctx context.Context, t AgentTemplate) error
	GetTemplate(ctx context.Context, agentType string) (AgentTemplate, bool, error)
	ListTemplateTypes(ctx context.Context) ([]string, error)

	// Agent records — point access keyed by agentID.
	RegisterAgent(ctx context.Context, r AgentRecord) error
	GetAgent(ctx context.Context, id string) (AgentRecord, error) // zero-value, no error, if not found — see note
	ListAgents(ctx context.Context) ([]AgentRecord, error)        // excludes Dismissed
	DismissAgent(ctx context.Context, id string) (bool, error)
	UpdateAgentStatus(ctx context.Context, id, status string) (bool, error)

	// Graph primitives — traversal (BFS/up-walk) stays in app code.
	ListChildren(ctx context.Context, parentID string) ([]AgentRecord, error)

	// Events — append-only list keyed by agentID.
	AppendEvent(ctx context.Context, agentID string, e events.Event) (events.Event, error)
	GetEvents(ctx context.Context, agentID string) ([]events.Event, error)

	// Consents — point access keyed by (userID, parentType, childType); list
	// access optionally scoped by userID.
	UpsertConsent(ctx context.Context, rec ConsentRecord) (ConsentRecord, error)
	FindConsent(ctx context.Context, userID, parentType, childType string) (ConsentRecord, bool, error)
	ListConsents(ctx context.Context, userID string) ([]ConsentRecord, error)
	RevokeConsent(ctx context.Context, id, userID string) (bool, error)
}
```

Notes on the shape:

- **`GetAgent` not-found behavior**: today's `getAgent` returns the zero value
  with no error when the id is unknown (handlers check `rec.AgentID == ""`).
  Keep that contract for the in-memory impl to minimize handler churn; a
  Dynamo impl returning `ErrNotFound` can be normalized to the same zero-value
  convention inside the adapter, or the interface can be tightened in a later
  pass. Phase 4 keeps the existing contract to avoid a second simultaneous
  refactor.
- **`ctx context.Context`** is added to every method even though the
  in-memory impl ignores it — durable backends need it for cancellation/
  tracing, and retrofitting context later would mean touching every call site
  twice.
- **Errors**: the in-memory impl never errors (maps can't fail), so it always
  returns `nil`. The interface still returns `error` because a durable backend
  can fail (network, throttling) — handlers gain a new "translate store error
  to 5xx" branch, which is new but small.
- **`ListChildren(parentID)`** is the new primitive that replaces `subtree`'s
  internal "scan all agents, build adjacency map" step. App code does:

  ```go
  func (s *registryApp) subtree(ctx context.Context, id string) ([]string, error) {
      root, err := s.store.GetAgent(ctx, id)
      if err != nil { return nil, err }
      if root.AgentID == "" { return nil, nil }
      out, seen, queue := []string{}, map[string]bool{}, []string{id}
      for len(queue) > 0 {
          cur := queue[0]; queue = queue[1:]
          if seen[cur] { continue }
          seen[cur] = true
          out = append(out, cur)
          children, err := s.store.ListChildren(ctx, cur)
          if err != nil { return nil, err }
          for _, c := range children {
              queue = append(queue, c.AgentID)
          }
      }
      return out, nil
  }
  ```

  `depth(id)` and the `chain` handler become loops of `GetAgent` calls — they
  already are, structurally; only the receiver and error plumbing change.
- **`consentExpiry`** stays as pure policy logic in app code — it calls
  `GetTemplate`, doesn't touch consent storage, and shouldn't be a `Store`
  method.

## Step-by-step implementation

1. **Define the `Store` interface** in `internal/registry/store.go` as above.
   Add doc comments describing each method's key shape (useful both for
   readers and as a head start on the DynamoDB mapping table below).

2. **Move the existing maps behind `inMemoryStore`.** Create
   `internal/registry/memstore.go` with an `inMemoryStore` struct that is
   today's `store` struct verbatim (four maps + `sync.RWMutex`), implementing
   every `Store` method by porting the existing method bodies almost
   unchanged — just add `ctx context.Context` params (ignored) and `error`
   return values (`nil`). `NewInMemoryStore()` replaces `newStore()`.

   - `ListChildren(ctx, parentID)`: new method — single pass over `s.agents`
     filtering `rec.ParentID == parentID`. (Today this work is buried inside
     `subtree`'s adjacency-map build; factoring it out here is the only
     *new* logic in this step.)
   - `ListTemplateTypes(ctx)`: new method — the loop currently inlined in the
     `GET /v1/templates` handler (`main.go:790-799`), moved behind the lock.

3. **Move graph traversal into app code.** In `cmd/registry/main.go`, replace
   `(*store).subtree` and `(*store).depth` with free functions (or methods on
   a small `app`/`registryApp` wrapper that holds a `Store`) implemented via
   `GetAgent` + `ListChildren` as sketched above. The `chain` handler's
   existing up-walk already only calls `getAgent` — repoint those calls at
   `Store.GetAgent` and thread `ctx`/error handling through.

4. **Introduce a thin `app` struct (or keep package-level functions) holding
   `store registry.Store`** so `buildMux`, `revokeNode`, `resumeNode`, and the
   traversal helpers all take/use a `Store` instead of `*store`. Minimal
   diff option: rename the parameter type from `*store` to `registry.Store`
   everywhere it's threaded today (`buildMux(s registry.Store, ...)`,
   `revokeNode(ctx, s registry.Store, ...)`, `resumeNode(...)`), since the
   current code already passes `s` around as a single handle.

5. **Route every handler through the interface.** Update each of the ~16
   handlers in `buildMux` (`main.go:332-799`) to call the new `Store` methods
   (`s.GetTemplate(ctx, ...)`, `s.RegisterAgent(ctx, ...)`, etc.) and handle
   the new `error` returns — for store errors, respond `500` with a logged
   message (durable-backend failures are the only new error path; the
   in-memory impl never triggers it, so this is forward-looking plumbing, not
   urgent hardening).

6. **Update `main()`** (`main.go:811-831`) to call
   `registry.NewInMemoryStore()` instead of `newStore()`.

7. **Update `main_test.go`** call sites (`newStore()` →
   `registry.NewInMemoryStore()`) and any direct field access on `*store` (the
   tests appear to only call `s.putTemplate(...)` etc., which become
   `s.PutTemplate(ctx, ...)` with `context.Background()`). Run
   `go test ./cmd/registry/... ./internal/registry/...` and fix fallout.

8. **Keep `consentKey`, `consentExpiry`, `revokeNode`, `resumeNode`,
   `referencesTenant`, `substitute`, `mustMarshal`** as free functions in
   `cmd/registry/main.go` (or move `consentKey`/`consentExpiry` into
   `internal/registry` alongside `ConsentRecord` if that reads better) —
   these are policy/derivation logic, not storage, and `consentExpiry` already
   depends on `GetTemplate` which is now a `Store` method taking `ctx`.

9. **Sanity pass**: grep for any remaining direct field access on a concrete
   `store`/`inMemoryStore` type outside `internal/registry/memstore.go` and
   `cmd/registry/main.go`'s construction site — there should be none; the HTTP
   layer should only ever see `registry.Store`.

## DynamoDB reference design (documented, not built this phase)

This section specifies a single-table design sufficient for a future
`internal/registry/dynamostore.go` implementing the same `Store` interface.

### Access patterns enumerated

| # | Access pattern | Current method | Frequency / shape |
|---|---|---|---|
| 1 | Get template by `agentType` | `GetTemplate` | point read, hot path (every spawn) |
| 2 | Put template | `PutTemplate` | rare (admin/CI) |
| 3 | List all template types | `ListTemplateTypes` | rare (dashboard) |
| 4 | Get agent record by `agentID` | `GetAgent` | point read, very hot (every check) |
| 5 | Register/replace agent record | `RegisterAgent` | one per spawn |
| 6 | List all non-dismissed agents | `ListAgents` | dashboard polling — needs a non-Scan path |
| 7 | Dismiss agent (flip flag) | `DismissAgent` | rare |
| 8 | Update agent status | `UpdateAgentStatus` | one per status transition |
| 9 | List an agent's children (`ParentID == X`) | `ListChildren` | one per BFS node during revoke/resume |
| 10 | Append event for agent | `AppendEvent` | hot, append-only |
| 11 | List an agent's events, in order | `GetEvents` | dashboard event stream |
| 12 | Upsert consent for `(user, parentType, childType)` | `UpsertConsent` | one per CIBA grant |
| 13 | Find consent by `(user, parentType, childType)` | `FindConsent` | point read, CIBA auto-approve path |
| 14 | List consents (optionally by user) | `ListConsents` | dashboard — needs a non-Scan path |
| 15 | Revoke consent by id (+ optional user scope) | `RevokeConsent` | rare |

### Single-table key schema

One table, `spawnly_registry`, partition key `PK` (string), sort key `SK`
(string):

| Entity | PK | SK | Notes |
|---|---|---|---|
| Template | `TEMPLATE#<agentType>` | `META` | whole `AgentTemplate` as a JSON attribute or native map |
| Agent record | `AGENT#<agentID>` | `META` | whole `AgentRecord`; `ParentID`, `Status`, `Dismissed` as top-level attributes for GSI/filter use |
| Event | `AGENT#<agentID>` | `EVT#<ts-nanos>#<eventID>` | append-only; sort key is naturally chronological — `Query` with no filter returns events in order |
| Consent | `CONSENT#<userID>#<parentType>#<childType>` | `META` | mirrors `consentKey`; `Revoked`, `UserID` as attributes |

This collapses access patterns 1–2, 4–5, 10–11, 12–13 into single-item
`GetItem`/`PutItem`/`Query` calls on the base table — no GSI needed for those.

### GSIs

**GSI1 — `ParentIndex`** (pattern 9, `ListChildren`):
- `GSI1PK = PARENT#<parentID>` (empty/absent for agents with no parent —
  DynamoDB omits items missing a GSI key from the index, which is fine: a
  root agent simply has no children to look up via this path anyway)
- `GSI1SK = AGENT#<agentID>`
- Written on every `RegisterAgent` (and left unchanged by `UpdateAgentStatus`/
  `DismissAgent`, which only touch the base item).
- `ListChildren(parentID)` = one `Query` on `GSI1PK = PARENT#<parentID>`.
  BFS for `subtree()` therefore costs one `Query` per visited node — the same
  shape as the in-memory impl's per-node map lookup, just over the network.

**GSI2 — `ListIndex`** (patterns 6 and 14, avoid Scans):
- Both `ListAgents` (all non-dismissed agents) and `ListConsents` (all/by-user
  consents) are currently full scans over their respective maps — fine at
  in-memory scale, fatal as a DynamoDB `Scan` at production scale (cost grows
  with total table size, not result size, and a `Scan` competes for the
  table's whole provisioned/on-demand capacity).
- Mitigation: give every agent record and consent record a **list key** GSI
  attribute:
  - Agents: `GSI2PK = "AGENTLIST#" + <listKey>`, where `<listKey>` defaults to
    a fixed constant (e.g. `"ALL"`) today but is **designed to become the
    tenant/consumer id** once multi-tenant listing is needed (see Risks
    below). `GSI2SK = AGENT#<agentID>`.
  - Consents: `GSI2PK = "CONSENTLIST#" + <listKey>` similarly (`<listKey>` =
    `userID` when listing one user's consents, or a fixed constant for the
    admin "all consents" view — this may need **two** GSI entries per consent
    item, written via two attributes, if both "all" and "by-user" listing must
    stay non-Scan; alternatively keep "all consents" as an explicitly
    rare/admin-only Scan and reserve the GSI for the hot by-user path).
  - `ListAgents()` excluding `Dismissed`: either filter client-side after the
    `Query` (small result sets) or maintain a second list-key variant that
    excludes dismissed items by removing their GSI2 attributes on dismiss
    (write amplification for a rarely-toggled flag — likely not worth it;
    client-side filter is simpler and the dataset per list-key is bounded).

- **Headline lesson**: DynamoDB is access-pattern-first. `ListAgents` and
  `ListConsents` are exactly the kind of "we'll just iterate the table"
  pattern that works trivially with maps and a `Scan`, but which DynamoDB
  punishes badly if you don't design a GSI for it *up front*. This table
  exists precisely so that decision is made now, on paper, rather than
  discovered in production.

### The eventual-consistency-vs-revoke wrinkle

**The problem.** `subtree()` — the basis of the cascading revoke/resume
feature (`POST /v1/agents/{id}/revoke`, `main.go:551-565`) — is implemented as
a BFS using `ListChildren(parentID)` at each step. Under DynamoDB, that's a
`Query` against **GSI1 (`ParentIndex`)**. All DynamoDB GSIs are updated
**asynchronously** relative to the base table and offer **no strongly
consistent read option** (unlike the base table, which supports
`ConsistentRead: true`).

Concretely: agent **P** spawns child **C**. `RegisterAgent(C)` writes C's base
item (with `ParentID = P`) and, asynchronously, propagates to GSI1 as
`GSI1PK = PARENT#P`. If a user fires `POST /v1/agents/P/revoke` in the small
window before that GSI1 propagation completes, the BFS's `ListChildren(P)`
query may **not yet return C** — C is silently excluded from the subtree,
**stays `active`, keeps its SpiceDB authorization, and is never revoked.**
This is a real security gap on the platform's headline revoke-cascade feature:
a just-spawned child can "outlive" a revoke of its parent.

**Mitigation (recommended).** After the GSI1 query returns candidate children
for a node, issue a **strongly-consistent `GetItem`** (or `BatchGetItem`) on
the base table for each candidate **and additionally re-run the GSI1 query
once more after a short delay**, OR — simpler and sufficient — treat the GSI1
query as a *candidate set* and combine it with a base-table consistency check
on the parent itself plus a brief re-query:

The practical, low-complexity mitigation:
1. Query GSI1 for children of each node in the BFS frontier (eventually
   consistent — may miss very recent writes).
2. After the BFS completes, **re-run the same GSI1 queries once more** for
   every node that was *itself* registered within the last N seconds (cheap:
   compare `RegisterAgent` timestamp, already stored on the agent item). A
   second pass after GSI propagation (typically sub-second, but document as
   "best-effort, not guaranteed") catches stragglers.
3. **Document the residual window explicitly** rather than over-engineer: even
   with a re-query pass, a pathological case (revoke fired in the exact
   instant of a spawn, GSI still not caught up after the re-query) remains
   theoretically possible. The acceptable mitigation for a *reference*
   architecture is: (a) the re-query pass, which closes the window from
   "always possible" to "vanishingly rare", and (b) note that a
   stronger-consistency design (e.g. storing the full children list as an
   attribute on the parent item, updated transactionally via
   `TransactWriteItems` at spawn time) is possible for deployments with a
   stricter SLA, at the cost of write-side complexity (every spawn now writes
   to both the child item and the parent's children-list attribute,
   transactionally).

**Call-out for the doc/spec, verbatim for implementers**: *"GSI-based
`ListChildren` is eventually consistent. A revoke fired immediately after a
spawn can miss the just-written child via the ParentID GSI, leaving that node
un-revoked. Mitigate with a strongly-consistent re-query pass for recently
registered nodes; for stricter guarantees, maintain a transactionally-written
children list on the parent item instead of relying on the GSI."*

## Files to touch

- `internal/registry/store.go` — **new**: `Store` interface definition.
- `internal/registry/memstore.go` — **new**: `inMemoryStore` +
  `NewInMemoryStore()`, ported from `cmd/registry/main.go:22-227` (the `store`
  struct and its methods, minus `subtree`/`depth` which move to app code, plus
  the new `ListChildren`/`ListTemplateTypes`).
- `cmd/registry/main.go` — remove the `store` struct and its methods
  (`22-227`); replace `*store` parameters with `registry.Store`; move
  `subtree`/`depth` logic to app-code traversal helpers built on
  `GetAgent`/`ListChildren`; update all handlers (`332-799`) to call `Store`
  methods with `ctx` and handle `error`; update `main()` (`811-831`) to call
  `registry.NewInMemoryStore()`.
- `cmd/registry/main_test.go` — update construction (`newStore()` →
  `registry.NewInMemoryStore()`) and any direct calls to former `*store`
  methods.
- `docs/saas/phase-4-persistence-store-interface.md` — this document (already
  written).

No changes expected to `internal/registry/types.go` or
`internal/registry/consent.go` — `AgentTemplate`, `AgentRecord`, `ConsentRecord`,
`ConsentDecision`, and `EvaluateConsent`/`FirstUncoveredScope` are unchanged;
they're the values the new `Store` interface moves around.

## Testing & acceptance criteria

- `internal/registry.Store` interface exists and is the **only** type the HTTP
  layer (`cmd/registry/main.go`) depends on for persistence — no remaining
  reference to a concrete map-based struct outside `memstore.go`.
- `inMemoryStore` implements `Store` (compile-time `var _ registry.Store =
  (*inMemoryStore)(nil)` assertion in `memstore.go`).
- All `buildMux` handlers go through `Store` methods; the one prior exception
  (`GET /v1/templates` reaching into `s.mu`/`s.templates` directly,
  `main.go:790-799`) is fixed via `ListTemplateTypes`.
- `subtree()` and `depth()` are implemented in app code using only
  `GetAgent`/`ListChildren` — no method on `Store` performs its own BFS/walk.
- `go test ./cmd/registry/... ./internal/registry/...` passes unchanged in
  intent — `main_test.go`'s assertions (template CRUD, registration tuples,
  revoke/resume cascade, consent flows, chain endpoint) all pass against
  `inMemoryStore` with no behavioral changes, only construction-site renames.
- `go vet` / `go build ./...` clean.
- No new external dependencies introduced (DynamoDB SDK is *not* added this
  phase — it's a design doc only).

## Risks & interactions

- **Phase 5 (consent broker state)**: Phase 5 will add more consent-related
  state (broker-side session/grant bookkeeping). Because `Store` already
  carries `UpsertConsent`/`FindConsent`/`ListConsents`/`RevokeConsent` as
  first-class methods (not buried in the HTTP layer), Phase 5 can either
  extend `ConsentRecord` (no interface change) or add new `Store` methods
  following the same point-access/list-access pattern established here. The
  single-table DynamoDB design's `CONSENT#...` key prefix and `ListIndex` GSI
  are designed with room for additional consent-shaped attributes.
- **Consumer-level tenancy**: the `ListIndex` GSI (`GSI2`) is described above
  with a `<listKey>` that defaults to a constant today. When multi-tenant
  listing becomes a real requirement (a consumer/tenant should only see its
  own agents/consents), `<listKey>` becomes `tenantID`/`consumerID` — a
  **data-shape change, not an interface change**, since `ListAgents`/
  `ListConsents` already take filtering parameters (`userID` for consents;
  `ListAgents` would gain an optional scope parameter). This is explicitly a
  later concern (see Out of scope), but the GSI design anticipates it so it
  doesn't require a second migration.
- **Error handling is new surface area**: today's in-memory store never fails,
  so handlers have no "store error" branch. Adding `error` returns to every
  `Store` method (for the sake of a future durable backend) means every
  handler gains a new, currently-dead code path (`if err != nil { 500 }`).
  Low risk (in-memory always returns `nil`), but it's new code that needs
  review for correctness even though it can't be exercised by
  `main_test.go` against `inMemoryStore`.
- **`GetAgent` not-found convention**: keeping the "zero value, no error"
  convention for not-found (rather than a sentinel `ErrNotFound`) is a
  pragmatic choice to minimize this phase's diff, but it's slightly awkward
  for a durable backend (which naturally returns "no such item" as a
  recoverable condition, not a zero value). Flagged for reconsideration if/when
  the DynamoDB implementation is actually built.

## Out of scope

- Building the DynamoDB `Store` implementation (`internal/registry/dynamostore.go`)
  — fully specified above, not implemented.
- Adding the AWS SDK / DynamoDB client dependency.
- Full multi-tenant consumer isolation (the `ListIndex` GSI's `<listKey>`
  generalization from a constant to a tenant/consumer id) — anticipated in the
  key design but not implemented; today's `ListAgents`/`ListConsents` keep
  their current (unscoped / userID-scoped) signatures.
- Migrating any existing deployed state — there is none (in-memory only
  today), so there's no data migration concern for this phase.
- Changing `AgentTemplate`/`AgentRecord`/`ConsentRecord`/`ConsentDecision`
  shapes in `internal/registry/types.go` and `consent.go`.
