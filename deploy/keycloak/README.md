# Keycloak realm (Terraform)

Declares the `spawnly` Keycloak realm — the OIDC configuration (clients, scopes,
audiences, roles, users, authentication flows, CIBA) that Keycloak, together
with the custom Spawnly SPI providers in `keycloak/providers/`, uses to serve
human login, attestation-based agent token minting, RFC 8693 delegation, and
CIBA spawn-consent. See `plans/keycloak-idp/plan.md` / `spec.md`.

Under Keycloak's fixed-frontend-hostname model (`KC_HOSTNAME` +
`KC_HOSTNAME_BACKCHANNEL_DYNAMIC`), tokens carry `iss` = the frontend origin
(on kind `http://localhost:8090/realms/spawnly`), while the token/JWKS
endpoints resolve to the in-cluster backchannel host
`http://keycloak:8080/realms/spawnly/...`.

## What it declares

- **Realm** `spawnly` (Keycloak-managed signing keys; refresh-token rotation +
  one-time-use).
- **Client scopes** — one per `ApiScope` in Config.cs, all *optional* (never
  default, never full-scope).
- **Audience mappers** — reproduce `ApiResources`: granting any scope of a
  resource adds that resource name to the token `aud` (`sample-api-a`,
  `sample-api-b`, `travel-tools`, `registry`, `orchestrator`).
- **`spawnly-roles` scope + flat `role` claim** — sourced from the user's
  realm-role membership, emitted in the id_token and the access token;
  requestable only by `dashboard` and `mobile`. Named `spawnly-roles` (not
  `roles`) to avoid colliding with Keycloak's built-in `roles` default scope.
  Because every client has `full_scope_allowed = false`, the `admin` realm role
  is also added to this scope's role-scope mappings (a
  `keycloak_generic_role_mapper`) — otherwise the realm-role mapper filters
  `admin` out of the token and `role` never appears.
- **Clients** — `dashboard` (confidential, code+PKCE), `mobile` (public PKCE),
  `orchestrator` + `idp-consent` (client-credentials, `registry.consent`), and
  the six machine clients `weather-monitor` / `chain-worker` / `travel-planner`
  / `flight-search` / `hotel-search` / `fx-converter` (client-credentials with
  a placeholder secret; 120s token lifespan where Config.cs sets it).
- **Users** — `root` (realm role `admin`, member of no org — the platform master
  admin), `alice` / `viewer` / `bob` (no realm role), created only when their
  password variable is non-empty (fail-closed). `alice` is the org admin of the
  demo `acme` org and `bob` of `globex` — each via a SpiceDB tuple written by the
  onboarding API, NOT a realm role (see below). `root` is the ONLY realm admin.
- **`org-admin` client** — a confidential client-credentials client the registry
  uses to drive Keycloak Organizations/Users (multi-tenancy). Its service account
  holds exactly two `realm-management` roles, `manage-users` + `manage-realm`
  (never the master/realm admin). It is the *only* client with
  `full_scope_allowed = true`, and deliberately so: a service-account token
  carries its assigned roles only when full scope is allowed, so with it `false`
  Keycloak filters the realm-management roles out and every admin call 403s.
  Least privilege is preserved by the role *assignment* (just those two roles),
  and the client lists no `roles`/`orchestrator:*` scope, so it cannot mint a
  human/agent-shaped token.

## Organizations — never edit membership in the Keycloak console

Organization membership is a **dual-write**: it lives in Keycloak (who is in the
org) AND in SpiceDB (the member-vs-admin authorization tuples), kept consistent
by the registry's org onboarding API (`POST /v1/orgs`, `.../members`), which is
the single choke point that writes both together with cleanup on partial
failure. **Do not add, remove, or re-role org members directly in the Keycloak
admin console** (or via raw admin REST): a console edit updates Keycloak but not
SpiceDB, silently desyncing authorization from membership (a "member" the
authz layer never sees, or a ghost admin tuple with no Keycloak membership). All
membership changes must flow through the Spawnly control-plane API — on kind,
`make reseed-orgs` (or the dashboard admin UI). Keycloak Organizations has no
native per-org role, so the admin/member distinction is SpiceDB's alone.

## Security invariant

`role` is never client-assertable: it is emitted only from an authenticated
user's realm-role membership, and only `dashboard`/`mobile` may request the
`roles`, `orchestrator:read`, and `orchestrator:write` scopes. Every client has
`full_scope_allowed = false`, and scopes are optional-only, so a machine client
requesting `roles`/`orchestrator:write` is granted nothing (a client-credentials
token has no user and thus no `role`). See the comment blocks in `scopes.tf` /
`clients.tf`.

## How it is applied

`scripts/bootstrap.sh` (Phase 0 extension) generates the `keycloak-admin`
Secret, brings up `deploy/manifests/keycloak.yaml`, port-forwards Keycloak, and
runs `terraform apply` here — feeding the generated values (dashboard client
secret, `dashboard-user` alice/viewer passwords, control-plane client secrets,
the machine placeholder secret) as Terraform variables.

**All secrets are Terraform variables — nothing is committed.** See
`terraform.tfvars.example` for the variable list; state and real `*.tfvars` are
gitignored.

## Local use

```sh
terraform init
terraform validate
terraform fmt -check
# apply happens via bootstrap.sh against a port-forwarded Keycloak.
```
