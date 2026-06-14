# Phase 5 — SpiceDB-native revoke & registry-native consent broker

## Overview

Phase 5 has two sub-parts that are grouped because they share a theme: **move
state ownership into the registry and SpiceDB, instead of leaning on
re-derivation from templates or on IdentityServer's CIBA machinery as the only
consent surface.**

- **Part A** replaces revoke/resume's write-all/delete-all-and-rederive
  pattern with a single boolean-shaped relationship per agent. Revoke and
  resume become O(1) tuple writes instead of O(relations-in-template) writes,
  and resume no longer depends on the agent's template still existing or being
  unchanged since registration.

- **Part B** moves the consent **state machine** (pending → approved/denied)
  into the registry itself, so a SaaS consumer without a CIBA-capable IdP can
  still get real user-in-the-loop consent through the registry's own API (and
  the dashboard). CIBA (Duende) becomes one *driver* that calls into the same
  broker instead of being the only path to a consent decision.

Both parts are about the registry becoming the single source of truth for
authorization *and* consent state, which is the core SaaS-readiness story for
this component: a consumer can swap SpiceDB schema details or swap CIBA for
their own IdP's consent UX, and the registry's contract (and the dashboard)
keeps working.

---

## Part A — Native revoke via status relationship

### Goal & why (heavy template re-derivation today)

Today:

- `revokeNode` (cmd/registry/main.go:278-292) deletes **every** SpiceDB
  relationship whose subject is `agent:<id>` via
  `sdb.DeleteAgentRelationships` (internal/spicedb/client.go:75-86), which
  issues a `DeleteRelationships` call filtered on
  `ResourceType: "tenant"` + `SubjectType: "agent"` + `SubjectId: id`.
- `resumeNode` (cmd/registry/main.go:298-327) re-reads the agent's template
  (`s.getTemplate(rec.AgentType)`), re-substitutes `{{agent_id}}` /
  `{{tenant_id}}` into every `SpiceDBRelationTemplate`
  (internal/registry/types.go:89-93), and re-writes each tuple with
  `sdb.WriteRelationship`.

Problems with this:

1. **Heavy and N-shaped.** A revoke/resume cycle does up to
   `len(tpl.AuthZ.SpiceDBRelations)` writes per agent, repeated for every node
   in a cascaded subtree (`subtree(id)`, used at cmd/registry/main.go:552 and
   :574).
2. **Re-derivation drift.** `resumeNode` reconstructs authority from
   `s.getTemplate(rec.AgentType)` *at resume time*. If the template was edited
   (or removed) between registration and resume, the resumed agent gets a
   **different** set of relationships than it had before revoke — silent
   authorization drift.
3. **`DeleteAgentRelationships` is schema-coupled.** It hardcodes
   `ResourceType: "tenant"` (internal/spicedb/client.go:78), so it only finds
   tuples on `tenant:*` resources. Any additional relation types a SaaS
   consumer adds to their schema bundle (Phase 2) are *not* cleaned up by
   revoke and *not* restored by resume, because resume only knows about
   `tpl.AuthZ.SpiceDBRelations` for the agent's own type — extension relations
   written by other paths are invisible to this mechanism entirely.

### Current state (file:line)

- Default schema bundle: `deploy/spicedb/schema.zed:1-6`
  ```
  definition agent {}

  definition tenant {
      relation agent: agent
      permission work_on = agent
  }
  ```
- Duplicated copy embedded as a Go string constant: `cmd/orchestrator/main.go:47-54`
  (same `definition tenant { relation agent: agent; permission work_on = agent }`).
  Both copies must change together until Phase 2's schema-ownership work lands
  (see Risks).
- Write path (registration): cmd/registry/main.go:437-464 — for each
  `tpl.AuthZ.SpiceDBRelations` entry, substitutes `{{agent_id}}` /
  `{{tenant_id}}` and calls `sdb.WriteRelationship(ctx, res, rel.Relation, sub)`.
- Revoke: cmd/registry/main.go:278-292 (`revokeNode`) — `DeleteAgentRelationships`
  + `s.updateAgent(id, "revoked")`.
- Resume: cmd/registry/main.go:298-327 (`resumeNode`) — re-derive from template
  + `WriteRelationship` per relation + `s.updateAgent(id, "active")`.
- Cascade endpoints: `POST /v1/agents/{id}/revoke` (cmd/registry/main.go:551-565)
  and `POST /v1/agents/{id}/resume` (cmd/registry/main.go:573-587) both call
  `subtree(id)` and then `revokeNode`/`resumeNode` per node.
- `spicedb.Client` interface: internal/spicedb/client.go:20-28 —
  `WriteSchema`, `WriteRelationship`, `DeleteAgentRelationships`,
  `CheckPermission`. `Mock` (internal/spicedb/client.go:108-144) backs unit
  tests; `CheckPermission` mock (internal/spicedb/client.go:139-144) only
  understands the `#agent@` tuple shape.
- "completed"/"failed" cleanup also calls `DeleteAgentRelationships`
  (cmd/registry/main.go:526-529) — terminal-state cleanup, distinct from
  revoke, but shares the same SpiceDB call and should be considered when
  changing its semantics (see Acceptance criteria).

### Target design (status-relation vs caveat; recommendation; schema change)

Two approaches were considered:

**(i) Status relation folded into the permission (recommended).**
Add a self-relation on `agent` that represents "enabled", and gate every
permission that currently resolves through `tenant#agent` on that relation
via intersection (`&`). Concretely:

```zed
definition agent {
    relation enabled_self: agent  // a single tuple: agent:<id>#enabled_self@agent:<id>
}

definition tenant {
    relation agent: agent
    permission work_on = agent & agent->enabled_self
}
```

The `agent->enabled_self` arrow walks from the `agent` relation on `tenant` to
the `enabled_self` relation on that same `agent` object. SpiceDB's `&`
(intersection) operator requires **both** operands to resolve for the
permission to hold — see SpiceDB docs on
[Permission Operators](https://authzed.com/docs/spicedb/concepts/schema#permissions):
`&` is intersection, `+` is union, `-` is exclusion. So:

- **Register**: write the template's relations (e.g.
  `tenant:<tid>#agent@agent:<id>`) **once**, plus **one** extra tuple
  `agent:<id>#enabled_self@agent:<id>`.
- **Revoke**: delete the **single** tuple `agent:<id>#enabled_self@agent:<id>`.
  `work_on` now evaluates to `agent ∩ ∅ = ∅` for that agent — permission denied
  — **without touching `tenant#agent`**.
- **Resume**: write the single tuple back. `work_on` re-evaluates to `agent ∩
  agent = agent` — permission restored, using the *original* `tenant#agent`
  tuple that was never deleted.

This generalizes to any permission a SaaS consumer's extended schema defines,
as long as each such permission includes `& agent->enabled_self` (or
transitively depends on a permission that does). The pattern is "every leaf
permission that should be revocable intersects with the agent's own enabled
flag" — a documented convention for schema authors (Phase 2 schema-ownership
docs should state this as the extension contract).

**(ii) SpiceDB caveat (CEL) on the relationship.**
Define a caveat:

```zed
caveat is_active(active bool) {
  active == true
}

definition tenant {
    relation agent: agent with is_active
    permission work_on = agent
}
```

The `agent` relationship is written once with the caveat context
`{"active": true}`. `CheckPermission` must then be called with
`Context: {"active": <bool>}` (or the registry must re-write the
*relationship's caveat context*, since SpiceDB caveat *context* is supplied
per-tuple at write time via `OptionalCaveat.Context`, not per-check — to
toggle it you `TOUCH` the same relationship with new caveat context). This
means revoke/resume still does a `WriteRelationships` TOUCH on the **same**
relation that encodes the template authority — i.e. it's a write either way,
and now every `CheckPermission` caller must thread caveat context through
(orchestrator, sample agents, sidecars), which is a wider blast radius.

**Recommendation: (i), the status relation.** It is:
- Simpler to reason about (no CEL, no per-check context threading).
- A pure additive schema change — existing `tenant#agent` tuples and
  `CheckPermission` call sites (which pass no context today) keep working
  unchanged; only the *permission definition* gains `& agent->enabled_self`.
- One tuple per agent, independent of how many relations the template wrote —
  revoke/resume cost becomes O(1) regardless of schema complexity.
- The `Mock` SpiceDB client (internal/spicedb/client.go:108-144) can model it
  with the same tuple-set representation it already uses, by special-casing
  the `enabled_self` relation in `CheckPermission`.

#### Schema change to the default bundle

`deploy/spicedb/schema.zed`:

```zed
definition agent {
    // Exactly one tuple per agent: agent:<id>#enabled@agent:<id>.
    // Present while the agent is active; absent while revoked.
    relation enabled: agent
}

definition tenant {
    relation agent: agent
    permission work_on = agent & agent->enabled
}
```

(Naming: `enabled` rather than `enabled_self` — shorter, and `agent->enabled`
reads naturally as "the agent's own enabled relation".)

`cmd/orchestrator/main.go:47-54`'s duplicated constant must be updated
identically until Phase 2 makes the registry the single owner of the schema
bundle and the orchestrator fetches/embeds it from there instead of
maintaining its own copy.

### Step-by-step implementation

1. **Schema bundle**: update `deploy/spicedb/schema.zed` and the duplicated
   constant in `cmd/orchestrator/main.go:47-54` to the new `agent { relation
   enabled: agent }` + `permission work_on = agent & agent->enabled` form.
   Confirm both are pushed via `WriteSchema` (internal/spicedb/client.go:54-57)
   on bootstrap — check how/where `WriteSchema` is currently invoked (likely
   registry or orchestrator startup) and ensure the new schema is what gets
   written.

2. **`internal/spicedb/client.go`**: no interface change needed for approach
   (i) — `WriteRelationship` and a new `DeleteRelationship` (singular, see
   next point) are enough. Add:
   ```go
   // DeleteRelationship removes a single tuple: resource#relation@subject.
   DeleteRelationship(ctx context.Context, resource, relation, subject string) error
   ```
   implemented via `DeleteRelationships` with a fully-specified
   `RelationshipFilter` (resource type+id, relation, subject type+id) — unlike
   `DeleteAgentRelationships`'s broad subject-only filter. Add the `Mock`
   counterpart (delete the exact `resource#relation@subject` key from
   `m.tuples`).

3. **Registration** (cmd/registry/main.go:437-464): after writing the
   template's relations, write the status tuple once:
   ```go
   if err := sdb.WriteRelationship(r.Context(),
       "agent:"+agentID, "enabled", "agent:"+agentID); err != nil { ... }
   ```
   Include it in the `tuples` slice / `spicedb_relations_written` event for
   observability.

4. **`revokeNode`** (cmd/registry/main.go:278-292): replace
   `sdb.DeleteAgentRelationships(ctx, id)` with
   ```go
   sdb.DeleteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id)
   ```
   Drop the `DeleteAgentRelationships` call from this path entirely — the
   template relations (`tenant#agent@agent:<id>`, plus any schema-extension
   relations) are **left in place**, deliberately, because they no longer gate
   the permission on their own.

5. **`resumeNode`** (cmd/registry/main.go:298-327): replace the entire
   template-iteration block (lines 309-320: `tpl, ok := s.getTemplate(...)`,
   the `for _, rel := range tpl.AuthZ.SpiceDBRelations` loop, and the
   tenant-skip logic) with a single write:
   ```go
   sdb.WriteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id)
   ```
   The `s.getTemplate(rec.AgentType)` lookup and its "unknown agent type"
   failure mode (lines 303-307) are removed — resume no longer depends on the
   template existing.

6. **Terminal-state cleanup** (cmd/registry/main.go:513-532, the `PATCH
   /v1/agents/{id}` handler): when status becomes `completed`/`failed`, decide
   whether to:
   - (preferred) call `DeleteRelationship(ctx, "agent:"+id, "enabled",
     "agent:"+id)` — same effect as revoke (permission denied), but leaves the
     template relations as a lineage record, consistent with revoke's new
     behavior, **or**
   - keep calling `DeleteAgentRelationships` for full cleanup of terminal
     agents (their relations will never be needed again, unlike a
     revoked/resumable agent's).
   Recommend the first option for consistency: terminal = permanently
   "disabled" via the same mechanism revoke uses; a separate garbage-collection
   pass (out of scope) can later sweep relations for agents dead longer than
   some retention window.

7. **Mock CheckPermission** (internal/spicedb/client.go:139-144): update to
   require both `resource#agent@subject` **and**
   `"agent:"+subjectID+"#enabled@"+subject` tuples present, mirroring the real
   schema's `&`:
   ```go
   func (m *Mock) CheckPermission(_ context.Context, resource, _ string, subject string) (bool, error) {
       m.mu.RLock()
       defer m.mu.RUnlock()
       agentKey := resource + "#agent@" + subject
       enabledKey := subject + "#enabled@" + subject // subject is "agent:<id>"
       return m.tuples[agentKey] && m.tuples[enabledKey], nil
   }
   ```

8. **Cascade endpoints** (cmd/registry/main.go:551-587): no signature changes
   — `revokeNode`/`resumeNode` keep their existing per-node call shape; the
   cost reduction (N writes → 1 write per node) falls out of steps 4-5
   automatically.

9. **Tests**: update internal/spicedb/client_test.go and any registry tests
   that assert on `DeleteAgentRelationships` being called during revoke/resume
   — assert on the new single-tuple delete/write instead. Add a test that
   resumes an agent **after its template has been deleted/changed** and
   confirms the resumed agent's permission set is identical to pre-revoke
   (this is the regression test for the re-derivation-drift bug being fixed).

### Acceptance criteria

- Revoking an agent performs exactly **one** SpiceDB write (a delete of the
  `enabled` tuple), regardless of how many relations its template defines.
- Resuming an agent performs exactly **one** SpiceDB write (re-adding the
  `enabled` tuple), with **no** template lookup and no dependency on the
  template still existing or matching what it was at registration time.
- After revoke, `CheckPermission(... "work_on" ...)` returns `false` for the
  agent; after resume, it returns `true` again — verified against real
  SpiceDB (not just the Mock).
- A cascaded `/v1/agents/{id}/revoke` and `/v1/agents/{id}/resume` over an
  N-node subtree perform exactly N SpiceDB writes total (one per node), down
  from N × (relations-per-template).
- `tenant#agent@agent:<id>` and any schema-extension relations survive a
  revoke/resume cycle unchanged — only the `enabled` tuple toggles.
- Existing revoke/resume HTTP contract (`POST /v1/agents/{id}/revoke`,
  `POST /v1/agents/{id}/resume`, response shape `{"revoked": [...]}` /
  `{"resumed": [...]}`) is unchanged.

---

## Part B — Registry-native consent broker

### Goal & why (decouple consent from CIBA-capable IdPs)

Today the registry **computes** consent decisions
(`internal/registry/consent.go`) and **stores** consent grants
(cmd/registry/main.go:682-744), but the **pending-request lifecycle** — "a
spawn needs consent, surface it to a human, wait for approve/deny" — lives
entirely inside Duende's CIBA backchannel-authentication machinery in
IdentityServer. A SaaS consumer who doesn't run Duende/CIBA (e.g. uses Auth0,
Okta, or a custom IdP with no CIBA support) currently has **no way** to get a
human-in-the-loop consent prompt through this stack — the registry has no
concept of a "pending" consent request at all.

Target: the registry owns the **full** consent lifecycle —
`pending → approved | denied`, with timestamps and an idempotent decision —
exposed via its own REST API and reusing the existing notifier webhook
contract (`consent_pending`). CIBA becomes a thin driver: Duende's
notification hook *creates* a pending request in the registry instead of
managing its own pending state, and Duende's completion path *calls the
registry's approve/deny*, which in turn completes the CIBA request as a
side-effect of the registry's decision (or, for non-CIBA consumers, the
registry's approve/deny is called directly, e.g. from the dashboard, and there
is no CIBA request to complete).

### Current state (file:line — what registry already owns vs what CIBA owns)

**Registry already owns:**
- `ConsentRecord` (internal/registry/consent.go:14-24): per-`(user,
  parentType, childType)` grant with `Scopes`, `GrantedAt`, `ExpiresAt`,
  `Revoked`.
- `EvaluateConsent` (internal/registry/consent.go:51-65): re-consent triggers —
  revoked, expired, or `FirstUncoveredScope` (scope escalation,
  internal/registry/consent.go:38-49).
- Storage: `s.consents map[string]registry.ConsentRecord`
  (cmd/registry/main.go:29), keyed by `consentKey(userID, parentType,
  childType)` (cmd/registry/main.go:119-121), with `upsertConsent` (126-137,
  **replaces** any prior record for the edge — confused-deputy protection: a
  different parent type wanting the same child type needs its own key).
- REST API:
  - `POST /v1/consents` (cmd/registry/main.go:682-712) — record/replace a
    grant; expiry derived from `s.consentExpiry(parentType, childType, now)`
    (cmd/registry/main.go:176-..., reads the parent template's `consentTTL`).
  - `GET /v1/consents` (714-717) — list, optionally filtered by `userId`.
  - `POST /v1/consents/{id}/revoke` (724-730) — `s.revokeConsent(id, userId)`,
    marks `Revoked: true`; live agents untouched (separate from
    `/v1/agents/{id}/revoke`).
  - `GET /v1/consents/check` (736-744) — `EvaluateConsent` against the stored
    record; **always 200**, used by CIBA's notification hook to decide
    auto-approve vs prompt.

**CIBA/IdentityServer currently owns (the gap):**
- **Pending-request existence and storage**: Duende's
  `IBackChannelAuthenticationRequestStore` / `IBackchannelAuthenticationInteractionService`
  — the registry has no record that a consent is "awaiting a human".
- **The decision to prompt vs auto-approve at request-creation time**:
  `CibaConsentNotificationService.SendLoginRequestAsync`
  (identityserver/CibaConsentNotificationService.cs:34-59) calls
  `_registry.CheckConsent(...)` (AgentRegistryClient.cs:58-73 →
  `GET /v1/consents/check`); if granted, calls
  `_completion.ApproveAsync(..., recordConsent: false)`; else falls through to
  `NotifyAsync` (CibaConsentNotificationService.cs:62-83), which POSTs
  `{"type": "consent_pending", user, parentType, childType, scopes,
  bindingMessage}` to `NOTIFIER_WEBHOOK_URL` if set.
- **Approve/deny completion**: `CibaCompletionService.ApproveAsync`
  (identityserver/CibaCompletionService.cs:47-77) calls Duende's
  `CompleteLoginRequestAsync`, then — if `recordConsent` —
  `_registry.RecordConsent(...)` (AgentRegistryClient.cs:80-95 →
  `POST /v1/consents`), then sweeps other pending requests for the same edge
  (`ResolvePendingForEdgeAsync`, CibaCompletionService.cs:91-123).
  `DenyAsync` (125-127) completes with no consented scopes.
- **Dev/manual completion surface**: `DevCibaEndpoints.cs` (dev-only,
  `DEV_CIBA_API=true`) and the session-authenticated `CibaConsentApi.cs` —
  both operate on Duende's pending-request store, not the registry.
- **Dashboard**: `cmd/dashboard/static/index.html` has a "Consents" modal
  (lines ~1005-1012) and a `#consent-banner` (line 971) for "Pending CIBA
  consent requests for the logged-in user" — currently this UI must be reading
  pending state from IdentityServer (via the webhook/dev endpoints), not the
  registry, since the registry has no pending-request concept yet.

### Target design (pending→approved/denied state machine + API; CIBA as optional driver; notifier webhook reuse)

#### New registry type: `ConsentRequest`

```go
// internal/registry/consent.go (additions)

type ConsentRequestStatus string

const (
    ConsentPending  ConsentRequestStatus = "pending"
    ConsentApproved ConsentRequestStatus = "approved"
    ConsentDenied   ConsentRequestStatus = "denied"
)

// ConsentRequest is one human-in-the-loop ask: "user U, may parent type P
// spawn child type C with these scopes?" Keyed by the same (user, parentType,
// childType) edge as ConsentRecord — at most one *open* (pending) request per
// edge; creating a new request while one is pending returns the existing one
// (idempotent re-notify, not a duplicate prompt).
type ConsentRequest struct {
    ID         string               `json:"id"`
    UserID     string               `json:"userId"`
    ParentType string               `json:"parentType"`
    ChildType  string               `json:"childType"`
    Scopes     []string             `json:"scopes"`
    BindingMessage string           `json:"bindingMessage,omitempty"`
    Status     ConsentRequestStatus `json:"status"`
    CreatedAt  time.Time            `json:"createdAt"`
    ResolvedAt *time.Time           `json:"resolvedAt,omitempty"`
    // ExternalRef carries an optional driver-specific id (e.g. the Duende
    // BackchannelUserLoginRequest.InternalId) so a driver can correlate the
    // registry's decision back to its own pending object. Opaque to the
    // registry.
    ExternalRef string `json:"externalRef,omitempty"`
}
```

#### New store state + keying

- `s.consentRequests map[string]registry.ConsentRequest` (by `ID`, a fresh
  UUID/ULID per request).
- An index `(userID, parentType, childType) -> open request ID` mirroring
  `consentKey` so "create pending" is idempotent: if an open (pending) request
  already exists for the edge, return it instead of creating a duplicate (this
  is what lets `ResolvePendingForEdgeAsync`'s sweep behavior become a registry
  concern instead of an IdentityServer one — see step 6 below).

#### New REST API (mirrors the existing `/v1/consents` shape)

- `POST /v1/consent-requests` — create (or return existing open) pending
  request.
  ```json
  { "userId": "...", "parentType": "...", "childType": "...",
    "scopes": ["..."], "bindingMessage": "...", "externalRef": "..." }
  ```
  Response: the `ConsentRequest` (201 if newly created, 200 if an open one
  already existed for this edge). **Side effect**: fires the notifier webhook
  (moved from CIBA — see "Notifier webhook reuse" below), unless the edge is
  already covered by a stored `ConsentRecord` (in which case short-circuit:
  immediately mark `approved`, call `upsertConsent` is *not* needed since a
  covering record already exists, and skip the webhook). This collapses
  `CibaConsentNotificationService.SendLoginRequestAsync`'s
  check-then-notify-or-approve logic into the registry.

- `GET /v1/consent-requests` — list, filterable by `?userId=` and/or
  `?status=pending`. Backs the dashboard's pending-consent banner directly
  from the registry instead of from IdentityServer.

- `GET /v1/consent-requests/{id}` — fetch one (for polling).

- `POST /v1/consent-requests/{id}/approve` — body `{"scopes": [...]}`
  (optional — defaults to the request's originally-requested scopes; a UI
  could allow narrowing). Effects:
  1. Set `Status = approved`, `ResolvedAt = now`.
  2. `upsertConsent(ConsentRecord{UserID, ParentType, ChildType, Scopes,
     GrantedAt: now, ExpiresAt: consentExpiry(...)})` — **this replaces
     `AgentRegistryClient.RecordConsent` / `POST /v1/consents`**, called
     internally now rather than as a second HTTP round-trip from IdentityServer.
  3. Sweep: find any **other** open `pending` requests for the same `(userID,
     parentType, childType)` edge whose scopes are now covered by the
     freshly-granted `ConsentRecord` (via `EvaluateConsent`), and mark them
     `approved` too, recording the same `ConsentRecord` update (idempotent —
     `upsertConsent` replaces, doesn't duplicate). **This replaces
     `CibaCompletionService.ResolvePendingForEdgeAsync`.**
  4. Return the updated `ConsentRequest`.

- `POST /v1/consent-requests/{id}/deny` — Effects: `Status = denied`,
  `ResolvedAt = now`. No `ConsentRecord` change (a denial does not revoke an
  *existing* unrelated consent for the edge — only the pending request is
  denied).

- `GET /v1/consent-requests/{id}/poll` (or just rely on `GET
  /v1/consent-requests/{id}` — polling is just repeated GET): a driver
  (CIBA or otherwise) polls this until `status != pending`.

#### Securing the consent endpoints (control-plane auth)

The consent-lifecycle endpoints (`/v1/consents*`, `/v1/consent-requests*`) are
called only by **control-plane services** — the orchestrator (proxying the
dashboard) and the IdP's CIBA driver — never by agents. They therefore sit
behind a dedicated auth seam, `internal/controlplane`, separate from the
agent-registration `internal/registrant` verifier (an agent proves an *agent*
identity; a control-plane caller proves a *service* identity). Per the platform
dependency-direction rule, no implementation calls a specific IdP — the OIDC
tier validates tokens against a *configured* JWKS.

Selected on the registry by `CONTROL_PLANE_AUTH`:

| Tier | Registry checks | Caller presents |
| --- | --- | --- |
| `none` (default) | nothing — open behind cluster network isolation (local demo) | nothing |
| `shared-secret` | `Authorization: Bearer <CONTROL_PLANE_TOKEN>` (constant-time) | the static token |
| `oidc` | a JWT against `CONTROL_PLANE_OIDC_JWKS_URL`, enforcing audience (`…_AUDIENCE`, default `registry`) + scope (`…_SCOPE`, default `registry.consent`) | a client-credentials access token |

Per-service env (must agree across all three):

- **registry** — `CONTROL_PLANE_AUTH`; for `oidc`: `CONTROL_PLANE_OIDC_JWKS_URL`,
  `_AUDIENCE`, `_SCOPE`.
- **orchestrator** — `CONTROL_PLANE_AUTH`; for `oidc`: `CONTROL_PLANE_TOKEN_URL`,
  `CONTROL_PLANE_CLIENT_ID` (`orchestrator`), `CONTROL_PLANE_CLIENT_SECRET`,
  `CONTROL_PLANE_SCOPE`. Uses an `oauth2` `clientcredentials.TokenSource`
  (auto-refresh).
- **identity-server** — `CONTROL_PLANE_AUTH`; for `oidc`: same client-credentials
  vars with `CONTROL_PLANE_CLIENT_ID=idp-consent`. The C# `ControlPlaneTokenHandler`
  fetches + caches the token. Two IdP clients (`orchestrator`, `idp-consent`),
  one `registry.consent` `ApiScope`, and a `registry` `ApiResource` (audience)
  are defined in `identityserver/Config.cs`.

This authenticates the *caller*; it layers on top of (does not replace) the
existing `userId` confused-deputy scoping on approve/deny, which the
authenticated dashboard asserts from the user's session. The deploy manifests
ship `CONTROL_PLANE_AUTH=none` with the `shared-secret`/`oidc` env commented
inline.

#### CIBA as an optional driver

CIBA's role shrinks to: *translate Duende's backchannel-authentication
protocol to/from the registry's consent-request API.*

- **On `SendLoginRequestAsync`**
  (identityserver/CibaConsentNotificationService.cs:34-59): instead of calling
  `CheckConsent` then `NotifyAsync` itself, call
  `POST /v1/consent-requests` with `externalRef = request.InternalId`. If the
  response comes back `status: approved` (registry short-circuited because a
  covering `ConsentRecord` already existed), call `_completion.ApproveAsync(...,
  recordConsent: false)` immediately — same as today's auto-approve path, just
  driven by the registry's response instead of a separate `CheckConsent` call.
  Otherwise the request is `pending` in the registry and Duende's own request
  also stays pending — the registry's webhook fire (see below) replaces
  `NotifyAsync`.

- **On human approval via Duende's own UX** (e.g. a future first-party
  IdentityServer consent page, or `CibaConsentApi.cs`'s session-authenticated
  flow): instead of `CibaCompletionService.ApproveAsync` calling
  `_registry.RecordConsent` directly, it calls
  `POST /v1/consent-requests/{id}/approve` (looking up the registry's request
  ID via `externalRef`, or the driver stores the registry's `ConsentRequest.ID`
  alongside Duende's `InternalId` when it created the request). The registry's
  approve does the `ConsentRecord` upsert *and* the pending-sweep — Duende's
  `ResolvePendingForEdgeAsync` (CibaCompletionService.cs:91-123) becomes
  redundant for edges driven this way, but can remain as a CIBA-local
  best-effort mirror during migration (or be deleted once the registry sweep
  is proven — see Out of scope).

- **`DenyAsync`** similarly calls `POST /v1/consent-requests/{id}/deny`.

- **Dev/manual endpoints** (`DevCibaEndpoints.cs`,
  `identityserver/CibaConsentApi.cs`): unchanged in shape, but their
  `approve`/`deny` handlers now also call the registry's
  `/v1/consent-requests/{id}/{approve,deny}` so the registry's view of the
  world stays authoritative even when approval happens through Duende's UI.

#### Non-CIBA driver (the actual point of this part)

A consumer with, say, Auth0:
1. Their agent-spawn path (orchestrator or sidecar) detects a missing/expired
   consent via `GET /v1/consents/check` (unchanged).
2. Instead of triggering a CIBA backchannel request, it calls `POST
   /v1/consent-requests` directly with the user/parent/child/scopes. No
   `externalRef` needed.
3. The registry's notifier webhook fires `consent_pending` (same payload shape
   as today) to whatever the consumer's `NOTIFIER_WEBHOOK_URL`-equivalent is.
4. The dashboard (or the consumer's own UI), reading `GET
   /v1/consent-requests?status=pending&userId=...` directly from the registry,
   shows an approve/deny UI and calls `POST
   /v1/consent-requests/{id}/approve|deny`.
5. The spawn-path's poll on the consent-request (or on `/v1/consents/check`
   once approved) sees `approved` and proceeds. No IdentityServer/CIBA
   involvement at any point.

#### Notifier webhook reuse

- Move the webhook POST (currently `CibaConsentNotificationService.NotifyAsync`,
  identityserver/CibaConsentNotificationService.cs:62-83) into the registry's
  `POST /v1/consent-requests` handler, keeping the **exact same payload shape**:
  ```json
  { "type": "consent_pending", "user": "...", "parentType": "...",
    "childType": "...", "scopes": [...], "bindingMessage": "..." }
  ```
  Config: registry reads `NOTIFIER_WEBHOOK_URL` from its own env (it already
  reads other env config in `cmd/registry/main.go`'s startup). Best-effort —
  log-and-continue on failure, exactly as the current C# implementation does.
  IdentityServer's copy of `NotifyAsync` is deleted once the registry's
  `POST /v1/consent-requests` is the only path that creates pending requests.

### Step-by-step implementation

1. **`internal/registry/consent.go`**: add `ConsentRequestStatus`,
   `ConsentRequest` (as above). Add a pure function
   `ShouldShortCircuit(rec ConsentRecord, found bool, requestedScopes []string,
   now time.Time) (granted bool, decision ConsentDecision)` that wraps the
   existing `EvaluateConsent` — used by `POST /v1/consent-requests` to decide
   immediate-approve vs pending, reusing the exact logic `GET
   /v1/consents/check` already exposes.

2. **`cmd/registry/main.go` store**: add `consentRequests
   map[string]registry.ConsentRequest` plus an open-request index
   `map[string]string` keyed by `consentKey(userID, parentType, childType)` →
   request ID (only while `status == pending`). Add methods:
   - `createConsentRequest(req registry.ConsentRequest) (registry.ConsentRequest,
     bool /*created vs existing*/)`
   - `getConsentRequest(id string) (registry.ConsentRequest, bool)`
   - `listConsentRequests(userID string, status string) []registry.ConsentRequest`
   - `resolveConsentRequest(id string, approve bool, scopes []string) (registry.ConsentRequest,
     bool)` — on approve, also calls `upsertConsent` and sweeps other pending
     requests for the same edge (mirrors `ResolvePendingForEdgeAsync`).

3. **New handlers** in `buildMux` (cmd/registry/main.go, alongside the
   existing `/v1/consents*` block at lines 682-744):
   - `POST /v1/consent-requests`
   - `GET /v1/consent-requests`
   - `GET /v1/consent-requests/{id}`
   - `POST /v1/consent-requests/{id}/approve`
   - `POST /v1/consent-requests/{id}/deny`

   `POST /v1/consent-requests` logic:
   ```go
   if rec, ok := s.findConsent(req.UserID, req.ParentType, req.ChildType); ok {
       if d := registry.EvaluateConsent(rec, req.Scopes, time.Now()); d.Granted {
           cr := s.createApprovedConsentRequest(req) // status already "approved"
           json 201 cr; return
       }
   }
   cr, created := s.createConsentRequest(...)
   if created {
       go notifyWebhook(cr) // best-effort, same payload as CIBA's NotifyAsync
   }
   json (201 if created else 200) cr
   ```

4. **Webhook helper**: add `notifyWebhook(cr registry.ConsentRequest)` in
   `cmd/registry/main.go`, reading `NOTIFIER_WEBHOOK_URL` from env (same env
   var name as IdentityServer used, for drop-in compatibility), POSTing the
   same JSON shape as identityserver/CibaConsentNotificationService.cs:69-77.

5. **IdentityServer — `AgentRegistryClient.cs`**: add
   - `CreateConsentRequest(userId, parentType, childType, scopes, bindingMessage,
     externalRef) -> ConsentRequest?` → `POST /v1/consent-requests`
   - `ApproveConsentRequest(id, scopes) -> ConsentRequest?` → `POST
     /v1/consent-requests/{id}/approve`
   - `DenyConsentRequest(id) -> ConsentRequest?` → `POST
     /v1/consent-requests/{id}/deny`
   Keep `RecordConsent` and `CheckConsent` for now (back-compat / direct use),
   but stop calling them from the CIBA completion path once step 6 lands.

6. **`CibaConsentNotificationService.SendLoginRequestAsync`**
   (identityserver/CibaConsentNotificationService.cs:34-59): replace the
   `CheckConsent` + `NotifyAsync` pair with
   `CreateConsentRequest(..., externalRef: request.InternalId)`. If the
   returned status is `approved`, call `_completion.ApproveAsync(...,
   recordConsent: false)` as today. Delete `NotifyAsync`
   (CibaConsentNotificationService.cs:62-83) — the registry now owns the
   webhook.

7. **`CibaCompletionService.ApproveAsync` /
   `ResolvePendingForEdgeAsync`** (identityserver/CibaCompletionService.cs:47-123):
   when `recordConsent` is true, instead of `_registry.RecordConsent(...)`,
   call `_registry.ApproveConsentRequest(registryRequestId, scopes)` — where
   `registryRequestId` is threaded through from step 6 (store it alongside the
   CIBA `InternalId`, e.g. in the request's `Properties` dictionary via
   `CibaRequestValidator`, or via a small in-memory map keyed by `InternalId`).
   Delete `ResolvePendingForEdgeAsync` (cmd/registry's
   `resolveConsentRequest` sweep now does this); `DenyAsync` similarly calls
   `_registry.DenyConsentRequest(registryRequestId)`.

8. **Dashboard** (`cmd/dashboard/static/index.html`): point the existing
   `#consent-banner` and "Consents" modal's pending-list at `GET
   /v1/consent-requests?status=pending&userId=...`, and wire its
   approve/deny actions to `POST /v1/consent-requests/{id}/approve|deny`. The
   existing "Spawn Consents" modal (granted-consents list,
   `GET /v1/consents`) is unchanged — it's listing `ConsentRecord`s, a
   different type from `ConsentRequest`s.

9. **Tests**: Go unit tests for the new store methods and handlers
   (idempotent create, short-circuit on existing covering consent, sweep on
   approve, deny doesn't touch `ConsentRecord`). C# tests (if any exist for
   `CibaConsentNotificationService`/`CibaCompletionService`) updated for the
   new registry calls — check for an existing identityserver test project
   before assuming none exist.

### Acceptance criteria

- A consent request can be created, approved, and denied entirely through
  `POST /v1/consent-requests`, `POST /v1/consent-requests/{id}/approve`,
  `POST /v1/consent-requests/{id}/deny` — verified with no IdentityServer/CIBA
  involvement (e.g. a Go integration test that drives the registry API
  directly).
- Approving a consent request results in a `ConsentRecord` via
  `upsertConsent` identical in shape to what `POST /v1/consents` produces
  today (so `GET /v1/consents/check` keeps working unchanged for all existing
  callers).
- The notifier webhook (`consent_pending`, same JSON shape) fires from the
  registry on `POST /v1/consent-requests` when no covering consent exists.
- The existing CIBA E2E flow (13/13 in-cluster tests per the CIBA consent
  design doc) still passes after CIBA is rewired to call
  `CreateConsentRequest`/`ApproveConsentRequest`/`DenyConsentRequest` — i.e.
  CIBA works as an optional driver on top of the new broker.
- `(user, parentType, childType)` keying and confused-deputy protection (a
  different parent type requesting the same child type gets its own
  consent-request/record) are preserved.
- Existing `/v1/consents*` endpoints (`POST /v1/consents`, `GET /v1/consents`,
  `POST /v1/consents/{id}/revoke`, `GET /v1/consents/check`) remain unchanged
  in contract — nothing that currently calls them breaks.

---

## Files to touch

**Part A:**
- `deploy/spicedb/schema.zed` — add `relation enabled: agent` to `definition
  agent`, change `permission work_on = agent` to `agent & agent->enabled`.
- `cmd/orchestrator/main.go:47-54` — mirror the schema constant change.
- `internal/spicedb/client.go` — add `DeleteRelationship` to `Client`
  interface + `realClient` + `Mock`; update `Mock.CheckPermission` for the `&`
  semantics.
- `internal/spicedb/client_test.go` — update/add tests for the new method and
  permission semantics.
- `cmd/registry/main.go`:
  - registration handler (~437-464): write the `enabled` tuple.
  - `revokeNode` (278-292): single `DeleteRelationship` call.
  - `resumeNode` (298-327): single `WriteRelationship` call, remove template
    re-derivation.
  - terminal-state PATCH handler (513-532): decide and implement cleanup
    semantics per step 6.
- Any registry/spicedb tests asserting on `DeleteAgentRelationships` during
  revoke/resume.

**Part B:**
- `internal/registry/consent.go` — add `ConsentRequest`,
  `ConsentRequestStatus`, short-circuit helper.
- `cmd/registry/main.go` — new store fields/methods + 5 new handlers + webhook
  helper + `NOTIFIER_WEBHOOK_URL` env read.
- `identityserver/AgentRegistryClient.cs` — new `CreateConsentRequest`,
  `ApproveConsentRequest`, `DenyConsentRequest` methods.
- `identityserver/CibaConsentNotificationService.cs` — rewire
  `SendLoginRequestAsync`, delete `NotifyAsync`.
- `identityserver/CibaCompletionService.cs` — rewire `ApproveAsync`/`DenyAsync`,
  remove `ResolvePendingForEdgeAsync`.
- `identityserver/CibaRequestValidator.cs` — possibly extended to carry the
  registry's `ConsentRequest.ID` alongside existing properties (for
  `externalRef` round-tripping).
- `identityserver/DevCibaEndpoints.cs`, `identityserver/CibaConsentApi.cs` —
  approve/deny handlers also call the registry.
- `cmd/dashboard/static/index.html` — pending-consent banner and modal wired
  to `/v1/consent-requests*`.

## Risks & interactions

- **5a depends on Phase 2 (schema ownership).** Today the schema bundle is
  duplicated (`deploy/spicedb/schema.zed` and `cmd/orchestrator/main.go:47-54`)
  and there's no single writer. The `& agent->enabled` change must land in
  *both* copies atomically, or the orchestrator and registry will disagree
  about whether `work_on` requires `enabled` — a window where one component's
  `WriteSchema` overwrites the other's version could re-enable a revoked agent
  (if the orchestrator pushes the old schema without the `&`) or break all
  permission checks (if only one side pushes the new schema and the relation
  doesn't exist yet). Sequence: land the schema-ownership consolidation
  (Phase 2) *before* or *atomically with* 5a, so there's one writer and one
  bundle.

- **5a composes with Phase 1 (delete path).** Phase 1's delete path
  (referenced as "DELETE not cascading" in prior work — killing a chain root
  leaves children orphaned) presumably also calls some SpiceDB cleanup. Check
  whether Phase 1's delete handler calls `DeleteAgentRelationships` — if so,
  decide whether delete should *also* just drop the `enabled` tuple (cheap,
  consistent) or do full cleanup (delete also removes `tenant#agent` etc.,
  since a deleted agent has no resume path and its tuples are pure garbage).
  Recommend: delete does full `DeleteAgentRelationships` (today's behavior,
  appropriate for "gone forever"); revoke does the new single-tuple delete
  (reversible). These are now *semantically different* operations and the
  schema change makes that difference meaningful — document it clearly in
  both handlers' comments.

- **Migration of existing live agents.** Any agent registered *before* the
  schema change has `tenant#agent@agent:<id>` but **no** `agent:<id>#enabled@agent:<id>`
  tuple. After the schema change, `work_on = agent & agent->enabled` evaluates
  to **false** for all pre-existing agents — a silent mass-revoke. Need either:
  (a) a one-time backfill that writes `enabled` tuples for every currently-
  `active` agent in the registry's store before/during the schema rollout, or
  (b) a migration script run as part of deploy. This is the single biggest
  operational risk in 5a and must be sequenced carefully (backfill *before*
  the new schema is pushed, or both atomically in the same maintenance
  window).

- **5b touches IdentityServer integration — preserve the REST contract.**
  `/v1/consents`, `/v1/consents/{id}/revoke`, `/v1/consents/check` must keep
  working exactly as today for any caller that doesn't yet know about
  `/v1/consent-requests` (back-compat during rollout, and for any external
  integrations already built against the documented `/v1/consents*` surface
  per the CIBA consent design doc).

- **Webhook double-fire during migration.** If both the registry's new
  `POST /v1/consent-requests` *and* IdentityServer's old `NotifyAsync` fire the
  webhook during a partial rollout, `consent_pending` notifications could be
  duplicated. Sequence: deploy the registry's consent-request API and webhook
  first (additive, dormant until called), then cut over
  `CibaConsentNotificationService` to call it (which also deletes
  `NotifyAsync`) in the same release — don't leave both live across releases.

- **CIBA E2E suite (13/13)** from the CIBA consent design must be re-run after
  5b's rewire — it's the regression suite for "CIBA still works as an optional
  driver."

- **In-memory store.** Both `s.consents` and the new `s.consentRequests` are
  in-memory maps in `cmd/registry/main.go` — a registry restart loses pending
  consent requests (an in-flight CIBA request would then poll forever / a
  non-CIBA UI would show nothing). This is a pre-existing limitation (not
  introduced by 5b) but 5b increases its visibility since pending state now
  matters more. Out of scope for this phase, but flag as a follow-up
  (persistent store) — consistent with "SaaS-ready reference architecture"
  framing where the registry's store backend should itself be pluggable.

## Out of scope

- Persisting `s.consents` / `s.consentRequests` to a durable store (still
  in-memory after this phase).
- Garbage-collecting SpiceDB relations for long-terminal agents (Part A
  step 6 leaves template relations in place for terminal agents; a sweep job
  is a separate piece of work).
- A first-party IdentityServer consent UI (Duende's own browser-based consent
  page) — 5b only rewires the *backchannel* (CIBA) and dev/API paths.
- Removing `RecordConsent`/`CheckConsent` from `AgentRegistryClient.cs` — kept
  for back-compat even after CIBA stops calling them in its primary path.
- Caveat-based (approach ii) revoke — documented as the alternative considered
  and rejected, not implemented.
- Schema-ownership consolidation itself (that's Phase 2's scope) — 5a only
  notes the dependency and the duplication risk.
- Multi-tenant scoping of `/v1/consent-requests` beyond what `/v1/consents`
  already does (no new tenant dimension introduced).
- Per-caller *authorization* in the control-plane auth seam: the OIDC tier
  authenticates the caller and checks a single shared `registry.consent` scope,
  but does not yet authorize the `orchestrator` vs `idp-consent` clients
  differently (e.g. restricting who may `approve`). An mTLS control-plane tier
  is also not implemented. Both are natural extensions of `internal/controlplane`.
- Extending control-plane auth to the non-consent registry endpoints
  (`/v1/agents` list/status, `revoke`/`resume`): the seam is reusable there but
  this phase gates only the consent lifecycle.
