# Phase 1 — Fix the SpiceDB delete leak

## Goal & why it matters

Make agent-relationship cleanup work for **any** consumer schema, not just one
whose authz resource type happens to be literally `"tenant"`. Today,
`DeleteAgentRelationships` hardcodes `ResourceType: "tenant"` into the SpiceDB
`RelationshipFilter`. Any agent template whose `spiceDbRelations` reference a
different resource type (e.g. `project:{{tenant_id}}`, `workspace:{{...}}`, or
multiple resource types) will register fine — tuples get written — but on
revoke or on completion/failure, `DeleteRelationships` silently filters on the
wrong type and deletes **nothing**. The agent record flips to
`revoked`/`completed`/`failed`, but the SpiceDB authority for that agent
remains live forever. This is a silent permission leak: the revoke-cascade
("real time deny") and the completed/failed cleanup path
(cmd/registry/main.go:526-530) both depend on this call succeeding.

This is blocking productization because the registry's authz-materialization
core (writing tuples derived from a template's `spiceDbRelations`) is meant to
be schema-agnostic — the registry doesn't know or care what a consumer calls
its tenancy resource. The delete path currently breaks that promise.

## Current state (with file:line evidence)

- **internal/spicedb/client.go:20-28** — the `Client` interface:
  ```go
  type Client interface {
      WriteSchema(ctx context.Context, schema string) error
      WriteRelationship(ctx context.Context, resource, relation, subject string) error
      DeleteAgentRelationships(ctx context.Context, agentID string) error
      CheckPermission(ctx context.Context, resource, permission, subject string) (bool, error)
  }
  ```
  `DeleteAgentRelationships` takes only an `agentID` — no information about
  which resource type(s)/objects the agent's relationships live on.

- **internal/spicedb/client.go:75-86** — `realClient.DeleteAgentRelationships`
  hardcodes the resource type:
  ```go
  func (r *realClient) DeleteAgentRelationships(ctx context.Context, agentID string) error {
      _, err := r.c.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
          RelationshipFilter: &v1.RelationshipFilter{
              ResourceType: "tenant",
              OptionalSubjectFilter: &v1.SubjectFilter{
                  SubjectType:       "agent",
                  OptionalSubjectId: agentID,
              },
          },
      })
      return err
  }
  ```
  Any tuple whose resource type isn't `"tenant"` is left in place. SpiceDB's
  `RelationshipFilter` requires `ResourceType` (it cannot be left empty/wildcard
  across types in a single call), so this single hardcoded filter cannot cover
  a consumer schema such as `project:{{tenant_id}}` or one that writes tuples
  against multiple resource types (e.g. both `tenant:` and `project:` in the
  same template).

- **internal/spicedb/client.go:105-144** — `Mock.DeleteAgentRelationships`
  (lines 124-134) is actually schema-agnostic today — it scans all stored
  tuples for `@agent:<id>` suffix regardless of resource type. **The mock does
  not reproduce the production bug**, so existing tests that exercise revoke
  via the mock pass even though the real client would fail for non-`tenant`
  resources. This is itself a test-fidelity gap worth closing.

- **cmd/registry/main.go:278-292** — `revokeNode` calls
  `sdb.DeleteAgentRelationships(ctx, id)` (line 283) as part of revoking a
  single node in the cascade.

- **cmd/registry/main.go:526-530** — the `PATCH /v1/agents/{id}` handler calls
  the same method when `req.Status` is `"completed"` or `"failed"`.

- **cmd/registry/main.go:443-459** — `POST /v1/agents` (registration) is where
  tuples are actually written: it iterates `tpl.AuthZ.SpiceDBRelations`
  (internal/registry/types.go:85-93 — `AuthZSpec.SpiceDBRelations []SpiceDBRelationTemplate`,
  each a `{Resource, Relation, Subject}` template string), skips
  tenant-referencing relations for global agents (`referencesTenant`,
  main.go:262-264), substitutes `{{agent_id}}`/`{{tenant_id}}`
  (`substitute`, main.go:254-257), writes each resulting tuple via
  `sdb.WriteRelationship`, and accumulates the resolved `(resource, relation,
  subject)` triples into `tuples` (main.go:442) — which it then only uses to
  emit a `spicedb_relations_written` event (main.go:460-464). **The resolved
  tuples are computed but not persisted anywhere the delete path can reach.**

- **internal/registry/types.go:89-93** —
  ```go
  type SpiceDBRelationTemplate struct {
      Resource string `json:"resource"` // e.g. "tenant:{{tenant_id}}"
      Relation string `json:"relation"` // e.g. "agent"
      Subject  string `json:"subject"`  // e.g. "agent:{{agent_id}}"
  }
  ```
  Resource is an arbitrary `"type:id"` string — the template owns the type
  name; the registry never assumes `"tenant"`.

- **agents/chain-worker/template.json:20-24** (representative example) — the
  only relation template used by current example agents is
  `{"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject":
  "agent:{{agent_id}}"}`. This is why the hardcoded `"tenant"` filter has gone
  unnoticed: every shipped template happens to use `tenant:` as its resource
  type.

## Target design (recommended option + why; alternatives considered)

**Recommended: Option (c)** — generalize `DeleteAgentRelationships` to accept
the set of resource types (or full resource refs) to filter on, computed by
the caller from the template's `spiceDbRelations` at the call site, the same
way registration already computes resolved tuples.

### Why (c)

- The registry already has everything it needs at call time: `revokeNode` and
  the PATCH handler both have `rec.AgentType` and can call `s.getTemplate(...)`
  to get `tpl.AuthZ.SpiceDBRelations`, exactly as registration and `resumeNode`
  do (main.go:309, main.go:443).
- It keeps `internal/spicedb.Client` schema-agnostic: the interface still only
  deals in `"type:id"` strings and an agent id — it never needs to know what
  "tenant" means. This preserves the dependency-direction rule (platform stays
  neutral; the registry, not the SpiceDB client, owns template semantics).
- No new persistent state. Avoids a migration/storage concern in this phase.
- SpiceDB's `DeleteRelationships` filter requires a single `ResourceType` per
  call — so deleting across N distinct resource types means N calls. Option
  (c) makes that fan-out explicit and visible at the call site, where the
  template (the source of truth for resource types) is already in scope.

### Alternatives considered

- **(a) Derive deletion from what register wrote, recomputed at call time
  without storing anything new** — this is effectively the same mechanism as
  (c) (recompute resource types from the template), but framed as "derive,
  don't store." In practice (a) and (c) converge: (c) is the interface change
  that makes (a) possible. We fold (a) into (c)'s implementation: the delete
  call site recomputes resource types from `tpl.AuthZ.SpiceDBRelations` rather
  than reading back any stored tuple list.

- **(b) Store the written tuples per-agent in the registry, delete precisely
  those on revoke/complete** — most precise (handles templates whose resource
  *id* depends on runtime data beyond `{{agent_id}}`/`{{tenant_id}}`, and
  templates that change between versions), but:
  - Adds persistent state (`map[agentID][]tuple]`) to the in-memory `store`
    that must survive process restarts to remain correct — currently the
    `store` is in-memory only (per main_test.go conventions), so this would
    either need to be added to existing snapshot/restore logic or accepted as
    "best effort, lost on restart" (degrading exactly the case Phase 1 is
    fixing).
  - Higher implementation cost for a problem that (c) already solves given the
    current template model (relations are fully determined by
    `{{agent_id}}`/`{{tenant_id}}` substitution, which the template + agent
    record already capture).
  - Worth revisiting if a future phase introduces relation templates with
    additional runtime-derived placeholders — note this as a follow-up trigger
    in "Risks & interactions."

**Decision: (c), with (a)'s "recompute from template" derivation as the
call-site implementation.** (b) is documented as the fallback if template
relations gain placeholders beyond `agent_id`/`tenant_id`.

### Shape of the interface change

Replace the single-resource-type assumption with a list of resource types to
filter on (one `DeleteRelationships` call per type, since SpiceDB requires
exactly one `ResourceType` per filter):

```go
// DeleteAgentRelationships removes every tuple whose subject is "agent:agentID"
// and whose resource is one of resourceTypes. Pass the set of resource types
// that the agent's template relations reference (e.g. {"tenant"} or
// {"tenant", "project"}); an empty slice is a no-op.
DeleteAgentRelationships(ctx context.Context, agentID string, resourceTypes []string) error
```

This keeps the signature minimal (just type names, not full tuples) — it's
enough to build a correct `RelationshipFilter{ResourceType: t,
OptionalSubjectFilter: {...agentID}}` per type, matching exactly what
`WriteRelationship` could have produced for that agent, without requiring the
client to parse template syntax.

## Step-by-step implementation

1. **internal/spicedb/client.go** — change the `Client` interface
   (lines 20-28): update `DeleteAgentRelationships`'s signature to
   `DeleteAgentRelationships(ctx context.Context, agentID string, resourceTypes []string) error`. Update the doc comment to explain `resourceTypes` and that an
   empty slice is a no-op.

2. **internal/spicedb/client.go** — rewrite `realClient.DeleteAgentRelationships`
   (lines 75-86): loop over `resourceTypes`, issuing one `DeleteRelationships`
   call per type with that type in `RelationshipFilter.ResourceType` and the
   same `OptionalSubjectFilter` as today. Collect/return the first error (or
   join errors with `errors.Join` so a failure on one type doesn't mask
   cleanup of the others — prefer `errors.Join` so a partial leak on one
   resource type is still reported but doesn't block deletion on the rest).
   Short-circuit (return nil) if `resourceTypes` is empty.

3. **internal/spicedb/client.go** — rewrite `Mock.DeleteAgentRelationships`
   (lines 124-134) to take the same `resourceTypes []string` parameter and
   **actually filter on it** (build a set from `resourceTypes`, and for each
   stored tuple key `resource#relation@subject`, delete only if the subject
   suffix matches `@agent:<id>` **and** the resource's type prefix (before
   `:`) is in the set). This closes the test-fidelity gap noted above — today
   the mock is more permissive than the real client; after this change it must
   mirror the real client's type-scoped behavior so tests catch a missing
   resource type in the caller's list.

4. **cmd/registry/main.go** — add a helper `relationResourceTypes(tpl
   registry.AgentTemplate, hasTenant bool) []string` near `referencesTenant`
   (around line 262-264): iterate `tpl.AuthZ.SpiceDBRelations`, skip relations
   that reference `{{tenant_id}}` when `hasTenant` is false (same rule as
   `referencesTenant` applies elsewhere), and collect the **resource type**
   (the part of `rel.Resource` before `:`, which is static template text, not
   a placeholder — so it never needs substitution) into a de-duplicated slice.
   This is the (a)-style "derive from the template" computation, shared by all
   three call sites below.

5. **cmd/registry/main.go** — `revokeNode` (lines 278-292): before/while
   calling `sdb.DeleteAgentRelationships`, look up the agent's template via
   `s.getTemplate(rec.AgentType)` (mirror the lookup pattern in `resumeNode`,
   line 303) and pass `relationResourceTypes(tpl, rec.TenantID != "")` as the
   new argument. If the template lookup fails (`!ok`), log and fall back to
   `[]string{}` (no-op delete) rather than failing the whole revoke — revoke
   must remain best-effort/idempotent as documented in the existing comment
   (lines 274-277).

6. **cmd/registry/main.go** — PATCH handler (lines 513-532): same pattern —
   before calling `sdb.DeleteAgentRelationships` on `completed`/`failed`, fetch
   `rec := s.getAgent(agentID)` (already implicitly available via
   `s.updateAgent`'s return, or re-fetch), `tpl, ok := s.getTemplate(rec.AgentType)`,
   compute `relationResourceTypes(tpl, rec.TenantID != "")`, and pass it.

7. **internal/spicedb/client.go** — update the `client_test.go`
   `TestMockWriteCheckDelete` test (lines 10-34) to pass
   `[]string{"tenant"}` to `DeleteAgentRelationships`, and add a new subtest
   (or new test function) `TestMockDeleteScopedToResourceType` that:
   - Writes two tuples for the same agent against **different** resource types,
     e.g. `tenant:t1#agent@agent:a1` and `project:p1#agent@agent:a1`.
   - Calls `DeleteAgentRelationships(ctx, "a1", []string{"tenant"})`.
   - Asserts the `tenant:` tuple is gone (`CheckPermission` on `tenant:t1`
     returns false) **and** the `project:` tuple survives (`CheckPermission`
     on `project:p1` still returns true) — proving the resource-type filter is
     respected, not just the subject suffix.

8. **cmd/registry/main_test.go** — add a new template fixture (inline JSON or
   Go struct literal, following existing test conventions around lines
   90-110/620-700) whose `spiceDbRelations` use a non-`"tenant"` resource type,
   e.g. `{"resource": "project:{{tenant_id}}", "relation": "agent", "subject":
   "agent:{{agent_id}}"}`. Add test(s) covering:
   - Register an agent of this type → `CheckPermission("project:<tenant>",
     ..., "agent:<id>")` is true.
   - `POST /v1/agents/{id}/revoke` (or the cascade entrypoint used elsewhere in
     the file) → `CheckPermission` on `project:<tenant>` becomes false.
   - Separately, register another agent of this type, then `PATCH
     .../status=completed` → same `CheckPermission` becomes false.
   These exercise steps 5/6/3 end-to-end through the mock.

9. Run `go build ./...` and `go test ./internal/spicedb/... ./cmd/registry/...`
   to confirm both the existing `tenant:`-based tests and the new
   non-`tenant` tests pass.

## Files to touch

- `internal/spicedb/client.go` — interface signature, `realClient` impl,
  `Mock` impl.
- `internal/spicedb/client_test.go` — update existing call, add new test.
- `cmd/registry/main.go` — new `relationResourceTypes` helper; update
  `revokeNode` and the PATCH `/v1/agents/{id}` handler call sites.
- `cmd/registry/main_test.go` — new non-`"tenant"` template fixture + revoke
  and complete/cleanup tests.
- No changes needed to `internal/registry/types.go` (the existing
  `SpiceDBRelationTemplate.Resource` already carries the type name as static
  prefix text) or to any `agents/*/template.json` (existing templates keep
  working unchanged — `relationResourceTypes` derives `["tenant"]` for them,
  identical to today's hardcoded behavior).

## Testing & acceptance criteria

- [ ] `internal/spicedb` unit test: writing tuples against two different
      resource types for the same agent, then calling
      `DeleteAgentRelationships(ctx, agentID, []string{"tenant"})`, deletes
      only the `tenant:`-typed tuple and leaves the other resource type's
      tuple intact (proves the filter is type-scoped on both `Mock` and, by
      contract, `realClient`).
- [ ] `cmd/registry` integration-style test: a template whose
      `spiceDbRelations` resource type is **not** `"tenant"` (e.g.
      `project:{{tenant_id}}`):
  - Registers successfully and `CheckPermission` on the `project:` resource
    returns true.
  - On `/revoke`, `CheckPermission` on that `project:` resource returns false
    (full cleanup — previously a no-op because the filter was hardcoded to
    `"tenant"`).
  - On `PATCH .../status=completed` (separate agent instance), the same
    `CheckPermission` returns false.
- [ ] All existing tests in `internal/spicedb` and `cmd/registry` continue to
      pass unchanged in behavior for `"tenant"`-typed templates (regression
      guard — current shipped agents must keep working identically).
- [ ] `go build ./...` succeeds (interface change is exhaustive — no other
      implementers of `spicedb.Client` exist besides `realClient` and `Mock`;
      confirm via `grep -rn "spicedb.Client" --include=*.go` before/after).

## Risks & interactions

- **Phase 5 / revoke model changes**: the user's roadmap notes a later phase
  revisits the revoke model. This Phase 1 change is intentionally narrow — it
  changes *how resource types for deletion are determined* (template-derived
  list passed in) without changing *when/why* revoke or complete/failed
  cleanup fires, or the cascade traversal logic in `revokeNode`'s callers.
  Phase 5 should be able to layer on top of (c)'s
  `DeleteAgentRelationships(ctx, agentID, resourceTypes)` signature — e.g. if
  Phase 5 introduces per-edge or per-scope revocation rather than whole-agent,
  it can compute a narrower `resourceTypes`/filter set using the same
  template-derived helper (`relationResourceTypes`) rather than re-deriving
  the SpiceDB plumbing. Flag for whoever picks up Phase 5: re-check that
  `relationResourceTypes` and the new `Client` signature still fit before
  extending them further.
- **Partial-failure semantics**: with multiple resource types, a
  `DeleteRelationships` call can now fail for one type while succeeding for
  another. Using `errors.Join` surfaces both but the registry's current
  handling (`log.Printf` + continue, lines 283-285 / 527-529) already treats
  this as best-effort; no behavior change needed beyond ensuring the log
  message includes which type(s) failed.
- **Template authoring**: if a future template's `spiceDbRelations` entries
  use a placeholder in the resource *type* position itself (not just the id),
  `relationResourceTypes`'s "type is static text before `:`" assumption breaks.
  Current templates never do this (type is always a literal like `tenant` or
  `project`); flag this constraint in the `SpiceDBRelationTemplate` doc comment
  (internal/registry/types.go:89-93) as part of this change so future template
  authors don't violate it silently.
- **Option (b) fallback trigger**: if a future phase introduces relation
  templates whose resource *id* depends on more than
  `{{agent_id}}`/`{{tenant_id}}` (e.g. a per-relation runtime value only known
  at registration time and not reconstructible later), `relationResourceTypes`
  alone won't be enough to build an exact subject+resource filter, and Option
  (b) (store resolved tuples per agent) should be revisited.

## Out of scope

- Hosted billing/control-plane work of any kind (per the overall
  productization framing — Phase 1 is purely the delete-leak fix).
- Pluggable SpiceDB backend abstractions beyond the interface signature change
  (e.g. supporting non-SpiceDB authz backends) — that is a larger
  "pluggable behind interfaces" effort tracked separately.
- Changes to `resumeNode` (main.go:298-327) — it only *writes* relationships
  and already correctly derives them from the template; no delete-side leak
  there.
- Changes to the cascade traversal / revoke endpoint structure itself
  (main.go:543+) — only the per-node `DeleteAgentRelationships` call and its
  arguments change.
- Persisting resolved tuples (Option (b)) — documented as a fallback, not
  implemented in this phase.
- Schema validation that template `spiceDbRelations` resource types actually
  exist in the SpiceDB schema written via `WriteSchema` — out of scope, a
  separate concern from the delete-filter leak.
