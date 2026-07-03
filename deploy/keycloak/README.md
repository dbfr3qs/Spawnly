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
- **Users** — `alice` (realm role `admin`) and `viewer` (no role), created only
  when their password variable is non-empty (fail-closed).

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
