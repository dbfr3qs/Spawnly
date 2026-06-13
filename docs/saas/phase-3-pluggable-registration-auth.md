# Phase 3 — Pluggable registration auth (Registrant verifier)

## Goal & why it matters

Today the registry's `POST /v1/agents` handler hardwires SPIFFE JWT-SVID
validation: it expects every registering agent to present a SPIRE-issued
JWT-SVID, validated against SPIRE's OIDC JWKS endpoint. That's the right
default for a SPIRE-native deployment, but it means anyone adopting the
registry as a standalone "agent lineage + delegation policy + authz
materialization" reference architecture is forced to also run SPIRE — even
if their fleet already has a perfectly good IdP (Auth0, Okta, Keycloak,
Azure AD, etc.) or relies on mTLS for service identity.

This phase introduces a **`Registrant` verifier abstraction** so "who is
allowed to register an agent, and what agentID do they get" becomes
pluggable and config-driven, with the current SPIFFE/SPIRE behavior
preserved as the default. This is the first step in making the registry
usable by consumers who don't run SPIRE, without changing anything for
consumers who do.

## Current state (file:line evidence)

- `cmd/registry/main.go:376-388` — the register handler:
  ```go
  mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
      rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
      if rawToken == "" {
          http.Error(w, "missing SVID", http.StatusUnauthorized)
          return
      }
      spiffeID, err := validator.Validate(r.Context(), rawToken, "registry")
      if err != nil {
          log.Printf("SVID validation failed: %v", err)
          http.Error(w, "invalid SVID", http.StatusUnauthorized)
          return
      }
      agentID := path.Base(spiffeID)
      ...
  ```
  Two SPIFFE-specific assumptions are baked in here:
  1. The bearer token is a JWT-SVID validated against SPIRE's JWKS with
     audience `"registry"`.
  2. `agentID` is derived by taking the last path segment of the SPIFFE ID
     (`spiffe://trust-domain/ns/.../agentType/agentID` → `agentID`). This
     `path.Base` trick only makes sense because SPIFFE IDs are URI paths;
     a generic OIDC `sub` claim or an mTLS cert SAN won't look like that.

- `internal/spiffe/validator.go:14-16` — the existing interface:
  ```go
  type SVIDValidator interface {
      Validate(ctx context.Context, token, audience string) (spiffeID string, err error)
  }
  ```
  `JWKSValidator` (lines 18-54) fetches SPIRE's OIDC JWKS with
  `InsecureSkipVerify: true` (line 27, since SPIRE's OIDC provider serves a
  self-signed cert in-cluster) and validates the JWT-SVID's signature,
  audience, and expiry, returning `tok.Subject()` (the SPIFFE ID) as a bare
  string. `MockSVIDValidator` (56-63) is a test double that returns a fixed
  `SpiffeID`/`Err`.

- `cmd/registry/main.go:822-826` — construction in `main()`:
  ```go
  validator, err := spiffe.NewJWKSValidator(ctx, spireJWKSURL)
  if err != nil {
      log.Fatalf("SVID validator init: %v", err)
  }
  ```
  and `cmd/registry/main.go:329` — `buildMux(s *store, sdb spicedb.Client,
  validator spiffe.SVIDValidator) *http.ServeMux` takes the validator as a
  constructor argument, which is how tests inject `MockSVIDValidator`.

- `cmd/registry/main.go:356-374` — the orchestrator-only preregister
  endpoint:
  ```go
  // Internal pre-registration endpoint — no SVID required.
  // Called by the orchestrator at spawn time so the agent appears in the UI
  // immediately with "pending" status rather than waiting for the sidecar to start.
  mux.HandleFunc("POST /v1/agents/preregister", func(w http.ResponseWriter, r *http.Request) {
      var rec registry.AgentRecord
      if err := json.NewDecoder(r.Body).Decode(&rec); err != nil { ... }
      if rec.AgentID == "" { ... }
      rec.Status = "pending"
      s.registerAgent(rec)
      ...
  })
  ```
  This handler performs **no authentication at all** — it trusts whatever
  caller can reach the registry's `:8080` HTTP port on the cluster network,
  and accepts a caller-supplied `AgentID` and `AgentType` directly into the
  store. In-cluster this is acceptable because network policy + SPIRE
  workload identity restrict who can reach the registry pod. In a SaaS
  posture where the registry may be reachable from less-trusted networks
  (or the deployment doesn't have an equivalent network boundary), this is
  an open door: any caller can preregister an arbitrary `agentID`/`agentType`
  pair, and a subsequent legitimate `POST /v1/agents` registration for that
  same `agentID` will be accepted (the conflict check at lines 411-418 only
  rejects `completed`/`failed`/`revoked` agents, not `pending` ones created
  by an attacker).

## Target design

### `Registrant` interface

A new package `internal/registrant` defines the abstraction:

```go
package registrant

import (
    "context"
    "net/http"
)

// Identity is the verified caller, decoupled from the credential format
// used to prove it.
type Identity struct {
    // AgentID is the registry-facing identifier for the agent being
    // registered — the primary key in registry.AgentRecord. Generalizes
    // today's path.Base(spiffeID).
    AgentID string

    // Subject is the raw verified identity string from the credential
    // (SPIFFE ID, OIDC "sub", or cert SAN) — kept for logging/auditing.
    Subject string

    // Issuer identifies which verifier produced this identity
    // ("spiffe-svid", "oidc", "mtls") — useful for audit logs and for
    // tenants that mix verifier types.
    Issuer string
}

// Verifier authenticates an inbound registration request and derives the
// agent's identity. Implementations may read the Authorization header,
// the TLS peer certificate (r.TLS.PeerCertificates), or both.
type Verifier interface {
    // Verify authenticates r and returns the caller's Identity, or an
    // error suitable for returning as 401 Unauthorized. Implementations
    // MUST NOT write to w; the caller (registry handler) owns the HTTP
    // response.
    Verify(ctx context.Context, r *http.Request) (Identity, error)
}
```

Key design points:

- `Verify` takes the whole `*http.Request`, not just a token string, so
  the mTLS implementation can read `r.TLS.PeerCertificates` while the
  bearer-token implementations read `r.Header.Get("Authorization")`. This
  is the generalization needed because "the credential" isn't always a
  bearer token.
- `Identity.AgentID` generalizes `path.Base(spiffeID)` — each
  implementation owns its own extraction rule (last SPIFFE path segment,
  configurable JWT claim, or cert SAN/CN), so the handler no longer knows
  or cares about the credential shape.
- The existing `spiffe.SVIDValidator` interface and `JWKSValidator` /
  `MockSVIDValidator` types are **not removed** — `registrant.SpiffeVerifier`
  wraps an `spiffe.SVIDValidator` and adapts its output to `Identity`. This
  keeps `internal/spiffe` and any other callers (e.g. sidecar token
  validation, if present) untouched.

### Implementations

1. **`registrant.SpiffeVerifier`** (default, preserves current behavior)
   - Wraps an `spiffe.SVIDValidator` (normally `*spiffe.JWKSValidator`).
   - `Verify`: extract `Bearer <token>` from `Authorization`, call
     `validator.Validate(ctx, token, "registry")`, get back the SPIFFE ID
     string.
   - `Identity.AgentID = path.Base(spiffeID)` (today's behavior, moved
     here verbatim).
   - `Identity.Subject = spiffeID`, `Identity.Issuer = "spiffe-svid"`.

2. **`registrant.OIDCVerifier`** (generic OIDC JWT)
   - Config: `issuer` (OIDC issuer URL, used to discover/derive a JWKS URL
     via `<issuer>/.well-known/openid-configuration` or a directly
     configured `jwksURL`), `audience` (expected `aud` claim, e.g. the
     tenant's registered API audience), `agentIDClaim` (name of the JWT
     claim holding the agent identifier — defaults to `"sub"`, but
     consumers may map a custom claim like `"agent_id"` or
     `"client_id"`).
   - Internally reuses the same `jwk.Cache` + `jwt.Parse` pattern as
     `JWKSValidator` (lines 30-49 of validator.go), but:
     - does **not** hardcode `InsecureSkipVerify` — real OIDC providers
       have valid public certs; TLS verification stays on by default,
       with an opt-in `insecureSkipTLSVerify` escape hatch for self-hosted
       IdPs with self-signed certs (mirroring, but not silently inheriting,
       the SPIRE behavior).
     - validates `aud` against the configured audience and standard claims
       (`exp`, `nbf`, `iss`).
   - `Identity.AgentID` = value of the configured `agentIDClaim` (string
     claim lookup via `jwt.Token.Get(claim)`); error if missing/non-string.
   - `Identity.Subject = tok.Subject()`, `Identity.Issuer = "oidc"`.

3. **`registrant.MTLSVerifier`** (sketch — identity from client cert)
   - Config: `agentIDSource` (`"san_uri"`, `"san_dns"`, or `"common_name"`)
     and optional `sanURIPrefix` / extraction pattern (e.g. strip a
     `spiffe://trust-domain/.../` prefix the same way `path.Base` does
     today, or a regex capture group for a custom URI scheme).
   - `Verify`: requires `r.TLS != nil && len(r.TLS.PeerCertificates) > 0`
     (i.e. the registry's HTTP server — or a fronting proxy/ingress that
     forwards verified client certs via a header — must be configured for
     `tls.RequireAndVerifyClientCert`). Reads the leaf cert
     (`r.TLS.PeerCertificates[0]`).
   - `Identity.AgentID` extracted per `agentIDSource`:
     - `san_uri`: first `URIs` SAN entry, with `agentIDSource` extraction
       rule applied (e.g. `path.Base`).
     - `san_dns`: first `DNSNames` entry.
     - `common_name`: `Subject.CommonName`.
   - `Identity.Subject` = the raw SAN/CN value used, `Identity.Issuer =
     "mtls"`.
   - Note: this implementation is a **sketch** for this phase — full
     support (including the proxy-forwarded-cert header variant, e.g.
     `X-Forwarded-Client-Cert` for ingress-terminated TLS) is scoped as a
     fast-follow. The interface and config surface are defined now so the
     selection mechanism is future-proof.

### Config-driven selection

New env vars on the registry (`cmd/registry/main.go`), read alongside the
existing `SPICEDB_*` / `SPIRE_JWKS_URL` ones:

| Env var | Default | Purpose |
|---|---|---|
| `REGISTRANT_VERIFIER` | `spiffe-svid` | One of `spiffe-svid`, `oidc`, `mtls`. |
| `SPIRE_JWKS_URL` | `http://spire-oidc/.well-known/jwks.json` | (existing) used when verifier=`spiffe-svid`. |
| `OIDC_ISSUER_URL` | `""` | Required when verifier=`oidc`. |
| `OIDC_JWKS_URL` | `""` | Optional override; if empty, derive from `OIDC_ISSUER_URL/.well-known/jwks.json` or OIDC discovery. |
| `OIDC_AUDIENCE` | `"registry"` | Expected `aud` claim. |
| `OIDC_AGENT_ID_CLAIM` | `"sub"` | JWT claim used as `Identity.AgentID`. |
| `OIDC_INSECURE_SKIP_TLS_VERIFY` | `false` | Opt-in for self-signed IdPs. |
| `MTLS_AGENT_ID_SOURCE` | `"san_uri"` | One of `san_uri`, `san_dns`, `common_name`. |

`main()` builds exactly one `registrant.Verifier` based on
`REGISTRANT_VERIFIER` and passes it into `buildMux` in place of (not
alongside) the current `spiffe.SVIDValidator` parameter.

## Step-by-step implementation

1. **Define the `Registrant` interface and `Identity` type** in a new
   package `internal/registrant/registrant.go`: `Identity` struct
   (`AgentID`, `Subject`, `Issuer`) and `Verifier` interface as shown
   above. No dependencies on `internal/spiffe` in this file — it's the
   neutral contract per the platform's dependency-direction rule (SDKs/
   adapters depend on the contract, not vice versa).

2. **Implement `registrant.SpiffeVerifier`** in
   `internal/registrant/spiffe.go`:
   - `type SpiffeVerifier struct { Validator spiffe.SVIDValidator }`
   - `func NewSpiffeVerifier(v spiffe.SVIDValidator) *SpiffeVerifier`
   - `Verify` reproduces the body of `main.go:377-388` (extract bearer
     token, call `Validator.Validate(ctx, token, "registry")`, then
     `agentID := path.Base(spiffeID)`), returning
     `Identity{AgentID: agentID, Subject: spiffeID, Issuer: "spiffe-svid"}`.
   - This is the only file allowed to import `internal/spiffe` — it's the
     adapter, per the dependency-direction rule.

3. **Refactor `POST /v1/agents` in `cmd/registry/main.go`** to use
   `registrant.Verifier` instead of `spiffe.SVIDValidator`:
   - Change `buildMux`'s signature (line 329) from
     `validator spiffe.SVIDValidator` to `verifier registrant.Verifier`.
   - Replace lines 377-388:
     ```go
     identity, err := verifier.Verify(r.Context(), r)
     if err != nil {
         log.Printf("registration auth failed: %v", err)
         http.Error(w, "unauthorized", http.StatusUnauthorized)
         return
     }
     agentID := identity.AgentID
     if agentID == "" {
         http.Error(w, "registrant verifier returned empty agentID", http.StatusInternalServerError)
         return
     }
     ```
   - Everything below (template lookup, conflict check, SpiceDB tuple
     writes, etc.) is unchanged — it already operates on the `agentID`
     string and doesn't care how it was derived.
   - Log `identity.Issuer`/`identity.Subject` alongside `agentID` in the
     existing registration log line for audit trail.

4. **Implement `registrant.OIDCVerifier`** in
   `internal/registrant/oidc.go`:
   - `type OIDCConfig struct { JWKSURL, Audience, AgentIDClaim string; InsecureSkipTLSVerify bool }`
   - `func NewOIDCVerifier(ctx context.Context, cfg OIDCConfig) (*OIDCVerifier, error)`
     — mirrors `spiffe.NewJWKSValidator` (validator.go:23-38): builds an
     `http.Client` (TLS verify on unless `InsecureSkipTLSVerify`), creates
     a `jwk.Cache`, registers `cfg.JWKSURL`, primes the cache.
   - `Verify`: extract bearer token, `jwt.Parse` with
     `jwt.WithKeySet(keySet)`, `jwt.WithAudience(cfg.Audience)`,
     `jwt.WithValidate(true)`. Then `claimVal, ok := tok.Get(cfg.AgentIDClaim)`;
     if `!ok` or not a string, return an error ("agent id claim %q missing
     or not a string"). Return
     `Identity{AgentID: claimStr, Subject: tok.Subject(), Issuer: "oidc"}`.
   - If `cfg.JWKSURL == ""`, add a small helper to derive it from
     `OIDC_ISSUER_URL + "/.well-known/openid-configuration"` (fetch and
     parse `jwks_uri`), or simply require `OIDC_JWKS_URL` to be set
     explicitly in v1 and document the discovery fetch as a fast-follow —
     **recommend the latter for this phase** to avoid adding an HTTP
     round-trip + discovery-doc parsing dependency; document
     `OIDC_JWKS_URL` as required when `OIDC_ISSUER_URL`'s
     `/.well-known/jwks.json` convention doesn't hold.

5. **Sketch `registrant.MTLSVerifier`** in `internal/registrant/mtls.go`:
   - `type MTLSConfig struct { AgentIDSource string }` (`"san_uri"` |
     `"san_dns"` | `"common_name"`).
   - `func NewMTLSVerifier(cfg MTLSConfig) *MTLSVerifier`
   - `Verify`: check `r.TLS != nil && len(r.TLS.PeerCertificates) > 0`,
     else return error "no client certificate presented". Extract per
     `cfg.AgentIDSource` as described in Target design. Return
     `Identity{AgentID: ..., Subject: ..., Issuer: "mtls"}`.
   - Document in a comment that enabling this verifier requires the HTTP
     server in `main()` to be started with
     `tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: <pool>}`
     — i.e. `http.ListenAndServe` (main.go:830) must become
     `http.ListenAndServeTLS` with a configured `*http.Server` for this
     verifier to ever receive `r.TLS.PeerCertificates`. Add a
     `MTLS_CLIENT_CA_PATH` env var and a startup check: if
     `REGISTRANT_VERIFIER=mtls` but no `MTLS_CLIENT_CA_PATH` is set,
     `log.Fatalf` at startup rather than silently accepting unauthenticated
     requests.

6. **Wire selection in `main()`** (`cmd/registry/main.go:811-831`):
   ```go
   var verifier registrant.Verifier
   switch v := getEnv("REGISTRANT_VERIFIER", "spiffe-svid"); v {
   case "spiffe-svid":
       jwksValidator, err := spiffe.NewJWKSValidator(ctx, spireJWKSURL)
       if err != nil {
           log.Fatalf("SVID validator init: %v", err)
       }
       verifier = registrant.NewSpiffeVerifier(jwksValidator)
   case "oidc":
       cfg := registrant.OIDCConfig{
           JWKSURL:               getEnv("OIDC_JWKS_URL", ""),
           Audience:              getEnv("OIDC_AUDIENCE", "registry"),
           AgentIDClaim:          getEnv("OIDC_AGENT_ID_CLAIM", "sub"),
           InsecureSkipTLSVerify: getEnv("OIDC_INSECURE_SKIP_TLS_VERIFY", "false") == "true",
       }
       if cfg.JWKSURL == "" {
           log.Fatalf("OIDC_JWKS_URL required when REGISTRANT_VERIFIER=oidc")
       }
       var err error
       verifier, err = registrant.NewOIDCVerifier(ctx, cfg)
       if err != nil {
           log.Fatalf("OIDC verifier init: %v", err)
       }
   case "mtls":
       if getEnv("MTLS_CLIENT_CA_PATH", "") == "" {
           log.Fatalf("MTLS_CLIENT_CA_PATH required when REGISTRANT_VERIFIER=mtls")
       }
       verifier = registrant.NewMTLSVerifier(registrant.MTLSConfig{
           AgentIDSource: getEnv("MTLS_AGENT_ID_SOURCE", "san_uri"),
       })
       // Note: actual mTLS termination wiring (ListenAndServeTLS + ClientCAs)
       // is a separate change to the http.Server setup at main.go:830 —
       // out of scope for this phase's interface work; mtls case here
       // documents the config contract and fails fast if misconfigured.
   default:
       log.Fatalf("unknown REGISTRANT_VERIFIER %q", v)
   }

   s := newStore()
   log.Println("registry listening on :8080")
   log.Fatal(http.ListenAndServe(":8080", buildMux(s, sdb, verifier)))
   ```

7. **Address the preregister endpoint's trust model**
   (`main.go:356-374`): keep it unauthenticated by default (in-cluster
   posture, orchestrator reaches the registry over the pod network — no
   behavior change), but:
   - Add a `PREREGISTER_SHARED_SECRET` env var (empty by default, i.e.
     current behavior unchanged). If set, the handler requires a header
     (e.g. `X-Registry-Preregister-Token: <secret>`) matching the configured
     secret, else `401`. This gives SaaS operators a cheap way to close the
     hole without requiring a full SVID/OIDC round trip for an
     orchestrator-internal call.
   - Document in code comments (and in this doc's Risks section) that the
     real fix — having the orchestrator authenticate via the same
     `Registrant` mechanism (e.g. orchestrator presents its own SPIFFE-SVID
     or OIDC client-credentials token, verified by the same
     `registrant.Verifier` with a relaxed agentID requirement since
     preregister supplies `AgentID` in the body) — is a fast-follow, not
     this phase, to avoid scope creep into orchestrator changes.

## Files to touch

- `internal/registrant/registrant.go` — new: `Identity`, `Verifier`.
- `internal/registrant/spiffe.go` — new: `SpiffeVerifier` wrapping
  `spiffe.SVIDValidator`.
- `internal/registrant/oidc.go` — new: `OIDCVerifier`, `OIDCConfig`.
- `internal/registrant/mtls.go` — new: `MTLSVerifier`, `MTLSConfig` (sketch).
- `internal/registrant/registrant_test.go` — new: unit tests for all three
  verifiers (table-driven, using `MockSVIDValidator` for the spiffe case
  and locally-signed JWTs / self-signed certs for oidc/mtls).
- `cmd/registry/main.go`:
  - `buildMux` signature (line 329): `spiffe.SVIDValidator` →
    `registrant.Verifier`.
  - `POST /v1/agents` handler (lines 376-388): replace direct
    `validator.Validate` + `path.Base` with `verifier.Verify`.
  - `POST /v1/agents/preregister` handler (lines 356-374): optional
    shared-secret check.
  - `main()` (lines 811-831): verifier construction switch on
    `REGISTRANT_VERIFIER`.
  - imports: add `internal/registrant`; `internal/spiffe` import retained
    (used only in the `spiffe-svid` case and inside
    `registrant/spiffe.go`).
- `cmd/registry/main_test.go` (if it exists; otherwise wherever
  `buildMux` is currently exercised with `MockSVIDValidator`) — update
  call sites to pass a `registrant.Verifier` (e.g.
  `registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{...})`, or a new
  `registrant.MockVerifier` for simpler test wiring).
- Helm chart / k8s manifests for the registry deployment (e.g.
  `deploy/.../registry-deployment.yaml` or equivalent) — add the new env
  vars (`REGISTRANT_VERIFIER` etc.) with defaults that preserve current
  behavior (`spiffe-svid`, existing `SPIRE_JWKS_URL`).
- Docs: this file, plus a short section in the registry's own README (if
  one exists) describing the pluggable verifier and how to configure each
  mode.

## Testing & acceptance criteria

- **Unit — spiffe path unchanged**: existing tests that construct
  `buildMux` with `spiffe.MockSVIDValidator` continue to pass after being
  updated to wrap it in `registrant.NewSpiffeVerifier(...)`. A registration
  request with a mock SPIFFE ID `spiffe://example.org/ns/default/sa/foo/bar`
  still yields `agentID == "bar"` (same as `path.Base` today).
- **Unit — OIDC verifier**:
  - Valid JWT signed by a test JWKS, with `aud` matching configured
    audience and the configured `agentIDClaim` present → `Verify` returns
    `Identity{AgentID: <claim value>, Issuer: "oidc"}`.
  - Wrong audience, expired token, bad signature, or missing
    `agentIDClaim` → error.
- **Unit — mTLS verifier (sketch-level)**: given a fabricated
  `*http.Request` with `TLS.PeerCertificates` set to a cert with a known
  URI SAN, `Verify` extracts the expected `AgentID` for each
  `AgentIDSource` mode; missing `r.TLS` → error.
- **Integration / acceptance**: a registry instance started with
  `REGISTRANT_VERIFIER=oidc`, `OIDC_JWKS_URL=<test IdP JWKS>`,
  `OIDC_AUDIENCE=registry`, `OIDC_AGENT_ID_CLAIM=sub` successfully accepts
  `POST /v1/agents` with `Authorization: Bearer <plain OIDC JWT>` from a
  non-SPIRE issuer (e.g. a locally-run OIDC test provider or a
  hand-signed JWT against a test JWKS), creates the `AgentRecord` keyed by
  the JWT's `sub` claim, and writes the expected SpiceDB relationships —
  i.e. the rest of the registration pipeline (template lookup, conflict
  check, SpiceDB writes, event emission) is verified to be agnostic to
  which verifier produced the `agentID`.
- **Regression**: a registry instance started with the default
  `REGISTRANT_VERIFIER=spiffe-svid` (or the env var unset) behaves
  identically to pre-Phase-3 — same JWKS URL env var, same `path.Base`
  derivation, same error responses/status codes for missing/invalid SVID.
- **Documentation**: `internal/registrant/registrant.go` carries doc
  comments on `Identity` and `Verifier` explaining the contract (especially
  that `Verify` must not write to the `http.ResponseWriter` and that
  `AgentID` must be non-empty on success), so future verifier
  implementations (e.g. API-key, JWT-from-header-only) can be added without
  touching `cmd/registry/main.go` beyond the `switch` in `main()`.

## Risks & interactions

- **Token minting stays out of scope.** This phase only changes how the
  registry *verifies* a presented credential and derives an `agentID` from
  it. It does NOT mint tokens, issue SVIDs, or implement an
  `sub=user/act=agent` delegation-token flow — that remains entirely the
  consumer's IdP's responsibility (SPIRE for the spiffe-svid default; the
  tenant's OIDC provider for the oidc verifier; a private CA for mtls). The
  registry's only job is "given a credential, who is this and what's their
  agent ID" — orthogonal to how that credential was obtained.
- **`AgentID` collisions across verifier types.** If a deployment ever
  needs to run multiple verifiers concurrently (not in this phase's scope —
  `REGISTRANT_VERIFIER` selects exactly one), two different credential
  types could theoretically produce the same `AgentID` string for different
  real-world agents. Not a risk for this phase (single verifier per
  deployment), but worth noting in the `Identity.Issuer` field's doc
  comment as the reason it exists — a future multi-verifier registry could
  namespace `AgentID` by `Issuer` if needed.
- **OIDC claim trust.** The `agentIDClaim` is attacker-influenced only to
  the extent the IdP allows — if a tenant's IdP lets end users set
  arbitrary custom claims on their own tokens, and `OIDC_AGENT_ID_CLAIM`
  points at such a claim, a caller could pick their own `agentID` and
  collide with/impersonate another agent's record. Document that
  `agentIDClaim` should point at an IdP-controlled, non-user-editable claim
  (e.g. `sub`, `client_id`, or a custom claim set only by a
  client-credentials/service-account flow) — this is a configuration
  responsibility of the operator, not something the registry can validate.
- **mTLS requires server-level TLS changes.** `registrant.MTLSVerifier`
  cannot function until `cmd/registry/main.go`'s `http.ListenAndServe(":8080",
  ...)` (line 830) is replaced with a TLS-terminating server configured for
  `tls.RequireAndVerifyClientCert`. This phase defines the verifier and its
  config contract and fails fast (`log.Fatalf`) if selected without the
  prerequisite CA config, but the actual server/TLS wiring is a follow-up
  change (likely bundled with whatever ingress/TLS-termination story the
  SaaS reference architecture adopts).
- **Preregister endpoint trust.** The optional
  `PREREGISTER_SHARED_SECRET` is a stop-gap, not a complete fix — it's a
  static shared secret with no rotation story. In a multi-tenant SaaS
  posture, a compromised secret lets any holder preregister arbitrary
  `agentID`/`agentType`/`tenantId` records, which then block legitimate
  registration via the 409-conflict check (lines 411-418 only guard
  `completed`/`failed`/`revoked`, not `pending`). Flagging this explicitly:
  closing this properly likely requires either (a) routing preregister
  through the same `Registrant` verifier with the orchestrator's own
  credential, or (b) tightening the conflict-check semantics so a `pending`
  record created by preregister can be safely overwritten/claimed by the
  first authenticated `POST /v1/agents` for that `agentID`, regardless of
  who created the `pending` row. Both are out of scope for this phase but
  should be picked up before this endpoint is exposed beyond a trusted
  cluster network.
- **Backward compatibility.** `internal/spiffe.SVIDValidator`,
  `JWKSValidator`, and `MockSVIDValidator` are unchanged — any other code
  depending on `internal/spiffe` directly (sidecar-side token validation,
  if any) is unaffected. The only breaking change is `buildMux`'s second
  parameter type, which is internal to `cmd/registry` and its tests.

## Out of scope

- **Token minting / issuance of any kind.** No new endpoints for issuing
  SVIDs, OIDC tokens, or mTLS client certs. The registry continues to be a
  pure *verifier* of credentials minted elsewhere.
- **`sub=user, act=agent` delegation-token semantics** (actor claims,
  on-behalf-of flows) — these live in the IdP/token-minting layer per the
  platform-agent-agnostic-contract; the `Registrant` abstraction only cares
  about the final `AgentID` it needs to key the registry record, not how
  delegation was expressed in the token.
- **mTLS server wiring** (`ListenAndServeTLS`, `ClientCAs`, ingress
  cert-forwarding headers) — `MTLSVerifier` is defined and config-gated
  but not load-bearing until a follow-up phase wires up TLS termination.
- **OIDC discovery document fetching** (`/.well-known/openid-configuration`)
  — `OIDC_JWKS_URL` must be configured explicitly in this phase; automatic
  JWKS-URL derivation from an issuer URL via discovery is a fast-follow.
- **Multi-verifier / per-tenant verifier selection** — `REGISTRANT_VERIFIER`
  is a single global setting for the registry process in this phase; routing
  different tenants to different verifiers (e.g. tenant A uses OIDC, tenant
  B uses SPIFFE) is not addressed.
- **Fixing the preregister endpoint's authn model** beyond the optional
  shared-secret stop-gap — full remediation is called out as a fast-follow
  in Risks & interactions.
- **Changes to SpiceDB relationship writes, consent records, or any other
  registry behavior downstream of `agentID` resolution** — this phase is
  scoped strictly to "how do we get a verified `agentID` from the inbound
  request."
