# Phase 2 — Registry-owned schema & template validation

## Goal & why it matters

The registry is becoming the productionized "authz materialization" service of
Spawnly: it turns agent templates into SpiceDB tuples. But today the SpiceDB
**schema** — the definitions and relations those tuples must conform to — is
owned by a *different* service (the orchestrator), hardcoded as a Go string
constant, and silently duplicated from a deploy-time file that nothing
enforces. A consumer adopting Spawnly as a reference architecture and writing
their own schema has no single place to look, no way to ask the registry what
schema is active, and — worst of all — no feedback when their template doesn't
match it: the tuple write just fails and is logged, while the API call returns
`201 Created`.

Phase 2 closes this gap:

1. The registry owns the schema lifecycle (default schema embedded, written to
   SpiceDB at registry boot, overridable, inspectable via API).
2. `POST /v1/templates` validates `authzTemplate.spiceDbRelations` against the
   active schema's definitions/relations and rejects (400) anything that
   references an unknown object type or relation, *before* it can ever produce
   a silently-dropped tuple.

This sets up Phase 5 (status/`enabled` relation additions to the schema)
cleanly: schema becomes a versioned, registry-managed artifact with one
override path, not two divergent hardcoded copies.

## Current state (file:line evidence)

- **Schema is hardcoded in the orchestrator, not the registry.**
  `cmd/orchestrator/main.go:47-54` defines:
  ```go
  const spicedbSchema = `
  definition agent {}

  definition tenant {
      relation agent: agent
      permission work_on = agent
  }
  `
  ```
  This is a verbatim copy of the canonical `deploy/spicedb/schema.zed` (6
  lines, same two definitions: `agent` with no relations, and `tenant` with
  `relation agent: agent` and `permission work_on = agent`). Nothing keeps
  these two copies in sync — a change to one silently diverges from the other.

- **Schema is written at orchestrator boot, with retry.**
  `cmd/orchestrator/main.go:532-542`: a 10-attempt retry loop (3s sleep
  between attempts) calls `sdb.WriteSchema(ctx, spicedbSchema)` because
  "SpiceDB may not be ready immediately on first start." `log.Fatalf` on
  attempt 10 if it never succeeds — this is a hard boot dependency for the
  orchestrator today, but conceptually has nothing to do with spawning
  workloads; it belongs to whichever service owns authz materialization.

- **Tuples are written by the registry, from template data.**
  `cmd/registry/main.go:443-459` (inside `POST /v1/agents`), the registry
  iterates `tpl.AuthZ.SpiceDBRelations` (the `AuthZSpec.SpiceDBRelations`
  field, `internal/registry/types.go:85-93` —
  `SpiceDBRelationTemplate{Resource, Relation, Subject string}` with
  `{{agent_id}}`/`{{tenant_id}}` placeholders), substitutes placeholders via
  `substitute()` (`cmd/registry/main.go:254-257`), skips tenant-referencing
  relations for global agents via `referencesTenant()`
  (`cmd/registry/main.go:262-264`), and calls
  `sdb.WriteRelationship(ctx, res, rel.Relation, sub)`.

  **So schema (orchestrator) and the tuples that must conform to it
  (registry) are owned by two different services.** A consumer who edits
  `deploy/spicedb/schema.zed` (the file that looks canonical) gets nothing —
  the orchestrator's hardcoded constant is what's actually applied.

- **Silent failure on template/schema mismatch.**
  `cmd/registry/main.go:451-453`:
  ```go
  if err := sdb.WriteRelationship(r.Context(), res, rel.Relation, sub); err != nil {
      log.Printf("spicedb write error: %v", err)
  }
  ```
  If a template's `spiceDbRelations` references a `resource`/`subject` object
  type or `relation` name that doesn't exist in the schema (e.g. a typo, or a
  consumer added a new definition to their template but forgot to add it to
  the schema), `WriteRelationship` returns an error from SpiceDB — and the
  registry just logs it. The HTTP response for `POST /v1/agents` still
  proceeds to `201 Created` (or whatever follows at line ~460+), and
  `POST /v1/templates` (`cmd/registry/main.go:332-340`) does **zero**
  validation at template-registration time — it just decodes JSON and calls
  `s.putTemplate(t)`. The agent is registered in the dashboard but has *no*
  real authz grant. This is exactly the kind of "looks fine, is actually
  half-broken" failure mode that's unacceptable in a reference architecture.

- **`internal/spicedb/client.go:54-57`** — `WriteSchema` already exists on the
  `Client` interface (`internal/spicedb/client.go:21`) and is implemented by
  both `realClient` (writes via `r.c.WriteSchema`) and `Mock`
  (`internal/spicedb/client.go:115`, currently a no-op). The plumbing to write
  a schema already exists; it's just invoked from the wrong service with a
  duplicated string.

## Target design

### Schema ownership

- The registry becomes the **sole writer** of the SpiceDB schema. The
  orchestrator's `spicedbSchema` const and its boot-time `WriteSchema`
  retry loop (`cmd/orchestrator/main.go:47-54`, `:532-542`) are removed
  entirely.
- The canonical default schema lives as a single file,
  `deploy/spicedb/schema.zed` (already exists, already correct — just
  currently unused by code). The registry `go:embed`s this file (or a
  registry-local copy under `cmd/registry/` if we want the embed directory
  scoped inside the registry's build context — see "Files to touch" for the
  tradeoff) as the **default** schema.
- On registry boot, the active schema (default, or an override — see below)
  is written to SpiceDB via `sdb.WriteSchema(ctx, schema)`, wrapped in the
  same 10-attempt/3s-sleep retry loop that the orchestrator used
  (`cmd/orchestrator/main.go:532-542`), relocated verbatim into
  `cmd/registry/main.go`'s `main()`.
- **Override mechanism**: an operator/consumer can supply their own schema via
  a file path env var, e.g. `SPICEDB_SCHEMA_PATH`. If set and the file exists,
  its contents become the active schema instead of the embedded default. This
  is a simple, dependency-free override — no need for a CRD or ConfigMap
  abstraction in this phase; in K8s a consumer just mounts a ConfigMap at that
  path. Document this clearly since it's the extension point Phase 5 will use
  (see Risks).
- **Versioning**: add a `SchemaVersion` string alongside the schema text
  (e.g. a small `//go:embed` companion `schema.version` file, or a constant
  next to the embed, default `"v1"`). Exposed via `GET /v1/schema` so
  consumers/tests can detect drift. Keep this lightweight — a free-form string
  the registry doesn't interpret, just reports.
- **`GET /v1/schema`** (new, public — no SVID required, like
  `GET /v1/templates/:type`) returns:
  ```json
  { "schema": "definition agent {}\n\ndefinition tenant {...}\n", "version": "v1", "source": "default" | "override" }
  ```
  `source` tells the caller whether the embedded default or an override file
  is active — useful for debugging "why did my template validation reject
  this?".

### Template validation

- On `POST /v1/templates`, after JSON-decoding the `AgentTemplate`, parse the
  *active* schema into a small in-memory model:
  ```go
  type schemaModel struct {
      // definitionName -> set of relation/permission names declared on it
      definitions map[string]map[string]bool
  }
  ```
- **Parsing approach**: SpiceDB's schema language (the `.zed` / CaC DSL) has
  no simple official Go parser exposed as a small dependency suitable here,
  but `github.com/authzed/spicedb/pkg/schemadsl/compiler` (already a transitive
  dependency via `authzed-go`/`spicedb` — confirm in `go.mod`) can compile a
  schema string into a `CompiledSchema` with `ObjectDefinitions`, each having
  `Relation`/`Permission` declarations by name. **Prefer this real compiler
  over a hand-rolled regex/string parser** — it's correct for comments,
  multi-line definitions, caveats, etc., and it's the same parser SpiceDB
  itself uses, so "validates against schema" means the same thing here as it
  does at `WriteSchema` time. If pulling in `schemadsl/compiler` turns out to
  add an unacceptable dependency footprint, fall back to a pragmatic
  line-oriented parser: regex for `^definition (\w+) \{` blocks and
  `relation (\w+):` / `permission (\w+) =` lines within each block — sufficient
  for the deliberately simple default schema and most consumer schemas, but
  call out in code comments that it's a simplification.
- **Validating `spiceDbRelations`**: for each
  `SpiceDBRelationTemplate{Resource, Relation, Subject}`
  (`internal/registry/types.go:89-93`):
  1. Strip the `{{agent_id}}`/`{{tenant_id}}` placeholder *and the colon before
     it* — i.e. `"tenant:{{tenant_id}}"` → object type `"tenant"`,
     `"agent:{{agent_id}}"` → object type `"agent"`. Only the **type** prefix
     (before `:`) is checked against `schemaModel.definitions`; the ID portion
     is never schema-relevant (it's resolved at tuple-write time to an actual
     agent/tenant UUID).
  2. Check `Resource`'s object type exists in `schemaModel.definitions`.
  3. Check `Relation` is a declared relation **or permission** name on that
     object type's definition (SpiceDB tuples are written against relations,
     but for validation purposes accept either — a template author might
     reasonably think of `work_on` as the thing they're granting, even though
     only `relation agent: agent` is the writable one; reject only if the name
     is in neither set, since writing a tuple for a permission name would fail
     at SpiceDB anyway and we want the *type+relation* check to be a strict
     superset of what SpiceDB would reject).
  4. Check `Subject`'s object type (same stripping rule) exists in
     `schemaModel.definitions`.
  5. If any check fails, respond `400 Bad Request` with a body describing
     which relation template, which field, and which unknown type/relation
     name caused the rejection — e.g.
     `{"error": "spiceDbRelations[1]: unknown relation \"contributes_to\" on definition \"tenant\""}`.
- This validation runs **before** `s.putTemplate(t)` — an invalid template is
  never stored, so `POST /v1/agents` can never reach the silent-log branch at
  `cmd/registry/main.go:451-453` for *this* class of error (schema mismatches
  found at template-registration time). The runtime `log.Printf` at line
  452-453 stays as a defense-in-depth fallback for transient SpiceDB errors
  (e.g. connection issues), which validation cannot catch.

## Step-by-step implementation

1. **Embed the default schema in the registry.**
   - Add `cmd/registry/schema.zed` as a copy-of-record of
     `deploy/spicedb/schema.zed` (Go embed directives can't reach outside the
     module's build the file lives in if go.mod boundaries are awkward across
     `cmd/registry` vs repo root — check first; if `deploy/` is embeddable
     directly from `cmd/registry/main.go` via a relative `//go:embed
     ../../deploy/spicedb/schema.zed`, prefer that single-source approach to
     avoid the duplication this phase is trying to remove. `go:embed` does
     **not** support `..` path traversal, so in practice this means either (a)
     moving `deploy/spicedb/schema.zed` to live under `cmd/registry/` (and
     symlinking or having `deploy/` reference it), or (b) keeping
     `cmd/registry/schema.zed` as the source of truth and having
     `deploy/spicedb/schema.zed` become a generated/copied artifact with a
     comment pointing at the registry copy. Pick (b) — minimal disruption to
     existing deploy tooling that may reference `deploy/spicedb/schema.zed`.)
   - In `cmd/registry/main.go`, add:
     ```go
     //go:embed schema.zed
     var defaultSchema string

     const defaultSchemaVersion = "v1"
     ```
   - Add a `cmd/registry/schema.version` file (or inline const, simpler) — use
     the inline const unless a future need for multiple bundled schema
     versions arises.

2. **Relocate the boot-time WriteSchema + retry loop into the registry, and
   remove it from the orchestrator.**
   - In `cmd/orchestrator/main.go`: delete `const spicedbSchema` block
     (lines 47-54) and the retry loop (lines 532-542). The orchestrator's
     `sdb` client (`cmd/orchestrator/main.go:527`) is still needed for
     whatever else it does with SpiceDB (check `buildMux` usage at line 138 —
     confirm what orchestrator-side SpiceDB calls remain, e.g. `CheckPermission`
     for spawn authorization, and keep those).
   - In `cmd/registry/main.go` `main()` (around line 811-830): after
     `sdb, err := spicedb.New(...)` (line 818), add the relocated retry loop,
     using `loadActiveSchema()` (step 4) to get the schema text:
     ```go
     activeSchema, schemaSource := loadActiveSchema()
     for i := 1; i <= 10; i++ {
         if err := sdb.WriteSchema(ctx, activeSchema); err == nil {
             break
         } else if i == 10 {
             log.Fatalf("WriteSchema failed after 10 attempts: %v", err)
         } else {
             log.Printf("WriteSchema attempt %d/10 failed, retrying: %v", i, err)
             time.Sleep(3 * time.Second)
         }
     }
     ```
   - This makes the registry, not the orchestrator, the hard-fail-on-boot
     dependency for SpiceDB schema readiness. Confirm K8s startup/readiness
     ordering: does the orchestrator depend on the registry being up before it
     spawns anything that needs SpiceDB checks? (Likely yes already, since
     registration happens via the registry.) Document this dependency
     direction explicitly in a comment.

3. **Add `GET /v1/schema`.**
   - In `buildMux` (`cmd/registry/main.go:329`), register:
     ```go
     mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
         w.Header().Set("Content-Type", "application/json")
         json.NewEncoder(w).Encode(map[string]string{
             "schema":  activeSchema,
             "version": activeSchemaVersion,
             "source":  schemaSource, // "default" | "override"
         })
     })
     ```
   - `activeSchema`/`activeSchemaVersion`/`schemaSource` need to be captured
     in `buildMux`'s closure or passed as parameters — `buildMux` already
     takes `(s *store, sdb spicedb.Client, validator spiffe.SVIDValidator)`;
     add a `schema schemaInfo` param (small struct bundling schema text,
     version, source, and the parsed `schemaModel` from step 5) so it's
     computed once in `main()` and threaded through, not recomputed per
     request.

4. **Override config: `SPICEDB_SCHEMA_PATH`.**
   - New function in `cmd/registry/main.go`:
     ```go
     // loadActiveSchema returns the schema text, version, and source ("default"
     // or "override"). If SPICEDB_SCHEMA_PATH is set and readable, its contents
     // become the active schema (version becomes "override" unless the file's
     // first line is a "# version: x" comment — keep simple: version is always
     // "custom" for overrides unless we add a convention later).
     func loadActiveSchema() (schema, version, source string) {
         if p := os.Getenv("SPICEDB_SCHEMA_PATH"); p != "" {
             b, err := os.ReadFile(p)
             if err != nil {
                 log.Fatalf("SPICEDB_SCHEMA_PATH=%s: %v", p, err)
             }
             return string(b), "custom", "override"
         }
         return defaultSchema, defaultSchemaVersion, "default"
     }
     ```
   - Fail fast (`log.Fatalf`) on an unreadable override path — a consumer who
     set this env var clearly intends an override; silently falling back to
     default would mask a typo'd path and re-create exactly the "looks fine,
     is broken" problem this phase fixes.

5. **Validation in `POST /v1/templates`.**
   - New file `internal/registry/schema.go` (or `internal/spicedb/schema.go` —
     prefer `internal/registry` since this is registry-specific validation
     logic, not a general SpiceDB client concern) with:
     ```go
     package registry

     // SchemaModel is a parsed view of a SpiceDB schema sufficient for
     // validating AuthZSpec.SpiceDBRelations against it.
     type SchemaModel struct {
         // definitions maps object-type name -> set of relation/permission names
         definitions map[string]map[string]struct{}
     }

     func ParseSchema(schema string) (*SchemaModel, error) { ... }

     // Validate checks every SpiceDBRelationTemplate's Resource/Relation/Subject
     // type+relation against the schema, ignoring {{agent_id}}/{{tenant_id}}
     // placeholders (only the type prefix before ':' is checked).
     // Returns a descriptive error naming the index, field, and unknown
     // type/relation on first failure.
     func (m *SchemaModel) Validate(spec AuthZSpec) error { ... }
     ```
   - Decide compiler-vs-regex approach (see Target design) and implement
     `ParseSchema` accordingly. If using `schemadsl/compiler`, check
     `go.mod`/`go.sum` for `github.com/authzed/spicedb` already being present
     as a transitive dep of `authzed-go`; if it's not a direct dep, `go get`
     it explicitly and pin a version compatible with the SpiceDB server
     version used in `deploy/`.
   - In `cmd/registry/main.go`'s `POST /v1/templates` handler
     (lines 332-340):
     ```go
     mux.HandleFunc("POST /v1/templates", func(w http.ResponseWriter, r *http.Request) {
         var t registry.AgentTemplate
         if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
             http.Error(w, err.Error(), http.StatusBadRequest)
             return
         }
         if err := schemaModel.Validate(t.AuthZ); err != nil {
             http.Error(w, err.Error(), http.StatusBadRequest)
             return
         }
         s.putTemplate(t)
         w.WriteHeader(http.StatusCreated)
     })
     ```
     where `schemaModel` is the parsed `*registry.SchemaModel` computed once in
     `main()` from `activeSchema` and threaded into `buildMux` (same param
     bundle as step 3).

6. **Update `internal/spicedb/client.go` Mock if needed.**
   - `Mock.WriteSchema` (`internal/spicedb/client.go:115`) is already a no-op
     — fine, since the registry's boot-time `WriteSchema` call against a real
     SpiceDB is what matters; tests using `Mock` don't need schema enforcement
     at the SpiceDB layer (validation happens earlier, in
     `POST /v1/templates`, against `SchemaModel`, independent of `Mock`).

7. **Update deploy manifests / docs.**
   - Remove any orchestrator env vars/config that were schema-specific (there
     don't appear to be any beyond the embedded const, but check
     `deploy/` manifests for `SPICEDB_*` env vars on the orchestrator
     Deployment and confirm the registry Deployment gets the same
     `SPICEDB_ENDPOINT`/`SPICEDB_PSK` it already needs for tuple writes — no
     new infra creds required, just the new responsibility).
   - Add `SPICEDB_SCHEMA_PATH` to the registry's deploy manifest as a
     commented-out example (mount point for a future ConfigMap), so the
     override path is discoverable without code-reading.

## Files to touch

- `cmd/orchestrator/main.go` — remove `const spicedbSchema` (lines 47-54) and
  the boot-time `WriteSchema` retry loop (lines 532-542); keep `sdb` client
  init if other SpiceDB calls remain in the orchestrator (verify via
  `buildMux` at line 138).
- `cmd/registry/main.go` — add `//go:embed schema.zed` + `defaultSchema`
  var/const; add `loadActiveSchema()`; relocate the `WriteSchema` retry loop
  into `main()`; add `GET /v1/schema` handler; add validation call in
  `POST /v1/templates` handler (lines 332-340); extend `buildMux` signature to
  receive schema info (text, version, source, parsed model).
- `cmd/registry/schema.zed` — new file, copy of
  `deploy/spicedb/schema.zed`'s current contents (canonical source of truth
  going forward).
- `deploy/spicedb/schema.zed` — becomes a reference copy with a header comment
  pointing at `cmd/registry/schema.zed` as the source of truth (or removed
  entirely if nothing in `deploy/` applies it directly — check for any
  `kubectl apply`/init-job referencing this file first).
- `internal/registry/schema.go` — new file: `SchemaModel`, `ParseSchema`,
  `(*SchemaModel).Validate(AuthZSpec) error`.
- `internal/registry/types.go` — no field changes expected; `AuthZSpec` /
  `SpiceDBRelationTemplate` (lines 85-93) are read-only inputs to `Validate`.
- `go.mod` / `go.sum` — possibly add `github.com/authzed/spicedb` (for
  `schemadsl/compiler`) if not already a resolvable transitive dependency.
- Deploy manifests under `deploy/` — confirm/update env vars for registry
  (SpiceDB endpoint/PSK already present for tuple writes) and orchestrator
  (remove now-unused schema-related config, if any existed beyond the const).
- Tests: `internal/registry/schema_test.go` (new) for `ParseSchema`/`Validate`;
  `cmd/registry/main_test.go` (existing — find via
  `find cmd/registry -name '*_test.go'`) extend for `POST /v1/templates`
  400-on-bad-schema and `GET /v1/schema` happy path.

## Testing & acceptance criteria

1. **Single source of truth**: `deploy/spicedb/schema.zed` and the registry's
   embedded schema are the same file (or one is a generated reference to the
   other) — `grep -r spicedbSchema cmd/` returns nothing; the orchestrator no
   longer contains schema text.
2. **`GET /v1/schema`** returns 200 with the active schema text, version
   string, and `source: "default"` when `SPICEDB_SCHEMA_PATH` is unset.
3. **Default end-to-end still works**: registering the existing example
   templates (chain-worker, travel-planner, etc. — `find` under `examples/` or
   `agents/` for template JSON fixtures) via `POST /v1/templates` still
   succeeds (200/201), and `POST /v1/agents` for those types still writes
   tuples successfully against the registry-written schema (verify via
   `sdb.CheckPermission` or a SpiceDB `ReadRelationships` call in an
   integration test).
4. **Unknown definition rejected**: `POST /v1/templates` with a template whose
   `spiceDbRelations[0].resource = "project:{{agent_id}}"` (an object type
   `project` not in the schema) returns `400` with a message naming `project`
   as the unknown type. Template is **not** stored — confirm via
   `GET /v1/templates/<type>` returning 404 afterward.
5. **Unknown relation rejected**: a template with
   `spiceDbRelations[0] = {resource: "tenant:{{tenant_id}}", relation:
   "contributes_to", subject: "agent:{{agent_id}}"}` (valid types, undefined
   relation `contributes_to`) returns `400` naming `contributes_to` and
   `tenant`.
6. **Placeholder-only fields don't trip validation**: a template whose
   relations correctly use `{{agent_id}}`/`{{tenant_id}}` against valid
   types/relations (the existing default templates) passes — confirms the
   placeholder-stripping logic isolates the type prefix correctly and doesn't,
   e.g., try to look up `{{tenant_id}}` itself as a type name.
7. **Override path**: setting `SPICEDB_SCHEMA_PATH` to a file containing an
   extended schema (e.g. adding a `definition project {}`) makes
   `GET /v1/schema` report `source: "override"` and that schema's text, and a
   template referencing `project:{{...}}` now validates successfully where it
   would have 400'd against the default.
8. **Unreadable override fails fast**: `SPICEDB_SCHEMA_PATH=/no/such/file`
   causes the registry to `log.Fatalf` at boot (process exits non-zero), not
   silently fall back to default.
9. Existing registry unit/integration tests (`cmd/registry/*_test.go`,
   `internal/registry/*_test.go`) continue to pass unmodified except where
   `buildMux`'s signature change requires updating call sites.

## Risks & interactions

- **Orchestrator no longer writes the schema — confirm nothing else depends on
  that ordering.** Today the orchestrator's boot blocks (`log.Fatalf` after 10
  retries) until SpiceDB has the schema. After this phase, *the registry*
  blocks on that instead. Check:
  - Does the orchestrator perform any SpiceDB operation (e.g.
    `CheckPermission` for spawn-time authz, referenced via `buildMux` at
    `cmd/orchestrator/main.go:138`) **before** the registry has had a chance
    to write the schema? If orchestrator and registry start concurrently and
    the orchestrator's first SpiceDB call races the registry's schema write,
    that call could fail against an unschema'd SpiceDB. Mitigation: either (a)
    K8s startup ordering/readiness probes already serialize this (registry
    typically comes up first since the orchestrator calls it for templates),
    or (b) the orchestrator's SpiceDB calls should themselves tolerate a
    "schema not found" error with bounded retry — check current behavior of
    `CheckPermission` against a schema-less SpiceDB (likely returns an error,
    not a panic, so this should degrade to "deny" rather than crash — confirm
    in `internal/spicedb/client.go:88-103`).
  - Any other service (sample-api, dashboard backend) that calls
    `sdb.WriteSchema` directly? `grep -rn "WriteSchema" --include=*.go .` to
    confirm orchestrator and registry are the only two call sites before this
    change.
- **Phase 5 will add an `enabled`/status relation to the default schema** —
  design the override mechanism so it composes with that, not against it:
  - The `SPICEDB_SCHEMA_PATH` override is a *whole-file replacement*, not a
    patch/merge. If Phase 5 adds e.g. `relation enabled: agent` (or similar)
    to `cmd/registry/schema.zed`, any consumer who has set
    `SPICEDB_SCHEMA_PATH` to their own full schema file will **not**
    automatically pick up that addition — their override schema becomes
    stale relative to the default. This is an inherent tradeoff of "bring
    your own full schema" vs. "extend the default." For this phase, document
    this explicitly (e.g. in the `GET /v1/schema` response or a doc comment):
    "override replaces, does not extend, the default schema; if you override,
    you own keeping pace with default-schema changes across phases."
  - Keep `defaultSchemaVersion` (`"v1"` in this phase) as the seam Phase 5 will
    bump (e.g. `"v2"` when the `enabled` relation is added) — `GET /v1/schema`
    already exposes this, so Phase 5 doesn't need new plumbing, just a new
    embedded `.zed` + version bump.
  - Because `SchemaModel.Validate` is driven by whatever schema is *actually
    active* (default or override), Phase 5's new relation will automatically
    be validatable once it lands in `cmd/registry/schema.zed` — no changes
    needed to the validation code itself, only to the embedded schema file.
    This is the main reason to centralize parsing in `internal/registry/schema.go`
    now rather than hardcoding relation names.
- **`WriteSchema` is additive/idempotent in SpiceDB** (schema updates merge —
  removing a definition that has live relationships is rejected by SpiceDB,
  but adding new definitions/relations to an existing schema is safe). This
  matters for the override case: an override schema must be a *superset*
  sufficient for SpiceDB to accept (SpiceDB itself will reject a schema
  update that would orphan existing tuples — this is a SpiceDB-side
  validation, separate from and in addition to the registry's template
  validation added here).
- **Dependency footprint**: if `schemadsl/compiler` pulls in a large chunk of
  the SpiceDB server codebase as a Go dependency, weigh that against the
  regex-based fallback. Given the registry is meant to be a lean reference
  service, prefer the smallest dependency that correctly parses `definition`/
  `relation`/`permission` declarations — evaluate both before committing.

## Out of scope

- Hosted/multi-tenant schema management UI (e.g. a dashboard page to edit
  schema) — `GET /v1/schema` is read-only this phase; no `PUT`/`POST
  /v1/schema`.
- Migrating *existing* SpiceDB tuples when the schema changes — schema
  evolution/migration tooling is a future phase.
- Validating templates against SpiceDB via a live dry-run/`ReadSchema` round
  trip — validation in this phase is purely local (`SchemaModel` parsed once
  at boot from the active schema text), not a per-request SpiceDB call. A live
  `ReadSchema`-based approach (always validate against whatever SpiceDB
  currently has, even if it diverges from the registry's embedded copy after
  manual `zed` CLI edits) is deferred — the registry is the schema's source of
  truth by construction in this design, so divergence shouldn't occur absent
  out-of-band edits.
- Phase 5's actual `enabled`/status relation addition — this phase only
  ensures the *mechanism* (embed + version + override + validation) is ready
  to receive it.
- Per-tenant or per-template custom schema fragments (e.g. "this agent type
  gets its own definition") — the active schema is global to the registry
  instance, not scoped per template/tenant.
- Removing/changing `cmd/registry/main.go:451-453`'s `log.Printf` fallback for
  `WriteRelationship` errors at agent-registration time — that stays as
  defense-in-depth for transient SpiceDB connectivity issues, which
  template-time validation cannot catch.
