# ===========================================================================
# Clients — ported from identityserver/Config.cs.
#
# HARDENING (security-critical, audited):
#   - full_scope_allowed = false on EVERY client: no client implicitly gets
#     every realm role/scope.
#   - Optional scopes are the ONLY scopes a client can request (nothing is
#     default beyond the base openid/profile), so a client is provably unable
#     to obtain a scope not in its own optional_scopes list.
#   - `roles`, `orchestrator:read`, `orchestrator:write` appear ONLY in the
#     dashboard and mobile clients' optional_scopes. The eight machine clients
#     (orchestrator, idp-consent, and the six agents) never list them, so they
#     cannot mint a token carrying `role` or orchestrator:read/write. Combined
#     with the user-sourced role mapper (scopes.tf), `role` is never
#     client-assertable.
# ===========================================================================

# ---- dashboard: confidential, authorization-code + PKCE (S256) ------------
resource "keycloak_openid_client" "dashboard" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = "dashboard"
  name      = "Spawnly Dashboard"

  access_type = "CONFIDENTIAL"
  # Standard (authorization code) flow + PKCE; no implicit, no direct grant,
  # no service account.
  standard_flow_enabled        = true
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false
  service_accounts_enabled     = false
  pkce_code_challenge_method   = "S256"

  client_secret = var.dashboard_client_secret

  valid_redirect_uris             = ["${var.dashboard_origin}/callback"]
  valid_post_logout_redirect_uris = ["${var.dashboard_origin}/"]

  # The backend keeps its short-lived orchestrator token fresh via an ONLINE
  # refresh token (this flag), bounded by the realm's SSO windows (realm.tf).
  # NOT offline_access: since Keycloak 26.1 (keycloak/keycloak#32650) an initial
  # login with offline_access deletes the online SSO session, which broke the
  # silent (prompt=none) org switch.
  use_refresh_tokens = true

  full_scope_allowed = false

  consent_required = false
}

resource "keycloak_openid_client_optional_scopes" "dashboard" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.dashboard.id

  # The built-in `organization` scope below is provisioned by Keycloak as a SIDE
  # EFFECT of the realm's organizations_enabled=true (realm.tf). Terraform's graph
  # only sees resource references, not that side effect, so make the ordering
  # explicit: attach optional scopes only AFTER the realm (and its organization
  # scope) exists, or a clean apply can race to "client scope not found". (The
  # client→realm reference already implies this edge; this states it explicitly.)
  depends_on = [keycloak_realm.spawnly]

  # NOTE: only `profile` is a Keycloak DEFAULT default-client-scope that must
  # NOT be listed here (the provider errors "already attached as a default
  # scope"). `offline_access` is deliberately NOT attached: the dashboard must
  # never mint offline tokens — an initial login with offline_access deletes the
  # online SSO session on Keycloak 26.1+ (keycloak/keycloak#32650), breaking the
  # silent org switch. Detaching it here makes that a hard guarantee even if a
  # future login request asks for the scope. The dashboard's refresh token is the
  # ONLINE one (use_refresh_tokens above), valid for the realm's SSO windows.
  optional_scopes = [
    # Built-in scope Keycloak provisions when organizations_enabled=true
    # (realm.tf). Requesting `organization` (or `organization:<alias>` for a
    # single active org) makes the org membership mapper stamp the `organization`
    # claim into the token — the source of the single-org token the dashboard
    # mints. OPTIONAL so today's org-less logins are byte-identical.
    "organization",
    keycloak_openid_client_scope.roles.name,
    keycloak_openid_client_scope.api["orchestrator:read"].name,
    keycloak_openid_client_scope.api["orchestrator:write"].name,
  ]
}

# sub = username. Keycloak's default `sub` is the user UUID, but the platform
# keys HUMAN identity on the username (orchestrator does
# strings.TrimPrefix(claims.User, "user:"); the sidecar sends
# login_hint="user:"+userID; the CIBA login-user resolver looks it up by
# username). A UUID sub would break consent keying, so we override `sub` =
# username for the human clients (dashboard + mobile). Machine/control-plane
# clients keep the default UUID sub; their user identity comes from the
# registry-driven identity mapper (sub=user:<id>), not this human sub override.
resource "keycloak_openid_user_property_protocol_mapper" "dashboard_sub_username" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.dashboard.id
  name      = "sub-username"

  user_property = "username"
  claim_name    = "sub"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# ---- mobile: public, authorization-code + PKCE, no secret ------------------
resource "keycloak_openid_client" "mobile" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = "mobile"
  name      = "Spawnly Mobile"

  access_type                  = "PUBLIC"
  standard_flow_enabled        = true
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false
  service_accounts_enabled     = false
  pkce_code_challenge_method   = "S256"

  valid_redirect_uris             = ["spawnly://auth"]
  valid_post_logout_redirect_uris = ["spawnly://auth"]

  # Refresh token with one-time-use rotation (Config.cs RefreshTokenUsage.
  # OneTimeOnly). Rotation/one-time-use is enforced realm-wide (realm.tf:
  # revoke_refresh_token + refresh_token_max_reuse = 0).
  use_refresh_tokens = true

  full_scope_allowed = false

  consent_required = false
}

resource "keycloak_openid_client_optional_scopes" "mobile" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.mobile.id

  # See the dashboard optional-scopes note: the built-in `organization` scope is
  # provisioned by the realm's organizations_enabled flip, so order this attach
  # after the realm exists to avoid a clean-apply "client scope not found" race.
  depends_on = [keycloak_realm.spawnly]

  # Only `profile` is a default default-scope (do not list it). `offline_access`
  # is a default-OPTIONAL scope and MUST be listed here or this authoritative
  # resource detaches it, killing the mobile long-lived session. See finding #3
  # and the dashboard note above.
  optional_scopes = [
    "offline_access",
    # Built-in organization scope for parity with the dashboard (see the note on
    # the dashboard client above). Lets the mobile app mint single-org tokens too.
    "organization",
    keycloak_openid_client_scope.roles.name,
    keycloak_openid_client_scope.api["orchestrator:read"].name,
    keycloak_openid_client_scope.api["orchestrator:write"].name,
  ]
}

# sub = username for the mobile client too (see dashboard_sub_username above).
resource "keycloak_openid_user_property_protocol_mapper" "mobile_sub_username" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.mobile.id
  name      = "sub-username"

  user_property = "username"
  claim_name    = "sub"

  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# ---- orchestrator: control-plane, client-credentials only ------------------
resource "keycloak_openid_client" "orchestrator" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = "orchestrator"
  name      = "Spawnly Orchestrator"

  access_type                  = "CONFIDENTIAL"
  service_accounts_enabled     = true
  standard_flow_enabled        = false
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false

  client_secret = var.orchestrator_client_secret

  full_scope_allowed = false
}

resource "keycloak_openid_client_optional_scopes" "orchestrator" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.orchestrator.id

  # registry.consent ONLY — provably cannot obtain roles/orchestrator:*.
  optional_scopes = [
    keycloak_openid_client_scope.api["registry.consent"].name,
  ]
}

# ---- idp-consent: control-plane, client-credentials only -------------------
resource "keycloak_openid_client" "idp_consent" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = "idp-consent"
  name      = "Spawnly IdP Consent Driver"

  access_type                  = "CONFIDENTIAL"
  service_accounts_enabled     = true
  standard_flow_enabled        = false
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false

  client_secret = var.idp_consent_client_secret

  full_scope_allowed = false
}

resource "keycloak_openid_client_optional_scopes" "idp_consent" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.idp_consent.id

  optional_scopes = [
    keycloak_openid_client_scope.api["registry.consent"].name,
  ]
}

# ---- org-admin: control-plane org directory client (client-credentials) ----
# The registry's org onboarding + membership API (multi-tenancy Phase 3) writes
# Keycloak Organizations/Users through THIS service-account client — the single
# dual-write choke point over Keycloak + SpiceDB. It is LEAST-PRIVILEGE: its
# service account is granted exactly three realm-management client roles
# (manage-users, manage-realm, manage-clients), never the master/realm admin. The
# secret arrives at runtime from the bootstrap-generated Secret (never baked into
# an image). The manage-clients role (org-subdomains Phase 2) is the narrowest
# widening that lets it edit the dashboard client's satellite redirect URIs.
resource "keycloak_openid_client" "org_admin" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = "org-admin"
  name      = "Spawnly Org Admin"

  access_type                  = "CONFIDENTIAL"
  service_accounts_enabled     = true
  standard_flow_enabled        = false
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false

  client_secret = var.org_admin_client_secret

  # EXCEPTION to the file-wide full_scope_allowed=false invariant: a
  # service-account (client-credentials) token only carries the account's
  # assigned roles when full scope is allowed — with it false, Keycloak filters
  # the realm-management roles OUT of the token and every admin call 403s
  # (verified against a live realm). Least privilege is preserved a different way
  # here: the service account is assigned ONLY manage-users + manage-realm +
  # manage-clients below, so full scope still admits just those three roles (plus
  # the harmless default realm roles). This client never lists roles/orchestrator:*
  # among any scope, so it cannot mint a human/agent-shaped token.
  full_scope_allowed = true
}

# The built-in realm-management client, whose roles authorize admin-REST calls.
data "keycloak_openid_client" "realm_management" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = "realm-management"
}

# manage-users: create users (unknown-member invite), find users, org member
# add/remove/list. manage-realm: organization CRUD. manage-clients: read + edit
# the dashboard client's redirect/post-logout URIs (org-subdomains Phase 2 —
# NOT covered by manage-users/manage-realm; without it the PUT /clients 403s).
# These three — and only these — are the minimal set that authorizes every
# org-directory operation.
resource "keycloak_openid_client_service_account_role" "org_admin_manage_users" {
  realm_id                = keycloak_realm.spawnly.id
  service_account_user_id = keycloak_openid_client.org_admin.service_account_user_id
  client_id               = data.keycloak_openid_client.realm_management.id
  role                    = "manage-users"
}

resource "keycloak_openid_client_service_account_role" "org_admin_manage_realm" {
  realm_id                = keycloak_realm.spawnly.id
  service_account_user_id = keycloak_openid_client.org_admin.service_account_user_id
  client_id               = data.keycloak_openid_client.realm_management.id
  role                    = "manage-realm"
}

# manage-clients: edit the dashboard OIDC client's redirect + post-logout URIs so
# org create/delete can register/remove each org's satellite callback
# (https://<slug>.<apex>/callback). This is the least-privilege widening for
# org-subdomains Phase 2 — it grants client CRUD across the realm, which is why
# the adapter only ever GET/PUTs the single dashboard client and never mints
# credentials from it.
#
# SECURITY CAVEAT (second-order): manage-clients is realm-WIDE. The org-admin
# TOKEN still cannot be human/agent-shaped (no orchestrator scopes; see above),
# but a holder of the org-admin SECRET could read any client's secret or re-point
# an agent client OFF the spawnly-attestation authenticator onto a known
# client-secret and mint agent-shaped tokens — bypassing SPIFFE/STS attestation.
# Keycloak has no narrower built-in realm-management role; the true least-
# privilege fix is fine-grained admin permissions scoped to ONLY the dashboard
# client (enable + scope if/when available). Until then: protect the
# bootstrap-generated org-admin secret as tightly as any minting credential and
# consider alerting on admin-REST client edits other than the dashboard client.
resource "keycloak_openid_client_service_account_role" "org_admin_manage_clients" {
  realm_id                = keycloak_realm.spawnly.id
  service_account_user_id = keycloak_openid_client.org_admin.service_account_user_id
  client_id               = data.keycloak_openid_client.realm_management.id
  role                    = "manage-clients"
}

# ===========================================================================
# Machine (agent) clients — confidential client-credentials authenticated by
# the custom "spawnly-attestation" client authenticator (Phase 1). An agent
# presents its SPIFFE JWT-SVID as a client_assertion; the SPI verifies it and
# resolves the client by client_id — there is NO client secret in this path
# (the authenticator is not secret-based). The verified {subject, agentId} is
# stored in client-session notes for the Phase 2 identity mappers.
#
# access_token_lifespan = "120" where Config.cs sets AccessTokenLifetime=120
# (chain-worker, flight-search, hotel-search, fx-converter). weather-monitor
# and travel-planner use the realm default.
#
# NONE list roles / orchestrator:read / orchestrator:write among their optional
# scopes — see the hardening note at the top of this file.
# ===========================================================================

locals {
  # clientId -> { lifespan (null = realm default), optional_scopes = [...],
  #               ciba (enable the CIBA spawn-consent grant on this client) }
  #
  # CIBA (Phase 4) is enabled on the CONSENT-GATED agent types only —
  # chain-worker, flight-search, hotel-search, fx-converter — matching the C#
  # Config.cs clients that ran the backchannel flow. weather-monitor (no consent)
  # and travel-planner (the fan-out orchestrator, not itself consent-gated) do
  # NOT get the CIBA grant.
  machine_clients = {
    "weather-monitor" = {
      lifespan        = null
      optional_scopes = [] # openid only (openid is built-in default)
      ciba            = false
    }
    "chain-worker" = {
      lifespan = "120"
      optional_scopes = [
        "sample-api-a:read",
        "orchestrator:spawn",
      ]
      ciba = true
    }
    "travel-planner" = {
      lifespan = null
      optional_scopes = [
        "orchestrator:spawn",
      ]
      ciba = false
    }
    "flight-search" = {
      lifespan = "120"
      optional_scopes = [
        "flights:read",
        "orchestrator:spawn",
      ]
      ciba = true
    }
    "hotel-search" = {
      lifespan = "120"
      optional_scopes = [
        "hotels:read",
        "orchestrator:spawn",
      ]
      ciba = true
    }
    "fx-converter" = {
      lifespan = "120"
      optional_scopes = [
        "fx:read",
        "orchestrator:spawn",
      ]
      ciba = true
    }
  }
}

resource "keycloak_openid_client" "machine" {
  for_each = local.machine_clients

  realm_id  = keycloak_realm.spawnly.id
  client_id = each.key

  access_type                  = "CONFIDENTIAL"
  service_accounts_enabled     = true
  standard_flow_enabled        = false
  implicit_flow_enabled        = false
  direct_access_grants_enabled = false

  # Phase 1: authenticate with the custom attestation SPI, NOT a client secret.
  # The provider id must match SpawnlyAttestationAuthenticator.PROVIDER_ID
  # (registered via META-INF/services in keycloak/providers). A client_secret is
  # still set (kept from var.machine_client_secret) but is never used on this
  # authenticator path — it exists only so the client is well-formed and to
  # avoid a spurious secret-regeneration diff.
  client_authenticator_type = "spawnly-attestation"
  client_secret             = var.machine_client_secret

  access_token_lifespan = each.value.lifespan

  # Phase 3: enable Keycloak's STANDARD token-exchange grant on every machine
  # client so an agent (the actor/child) can run RFC 8693 delegation against the
  # realm token endpoint. The custom SpawnlyTokenExchangeProvider (registered
  # with a higher order() than the built-in Standard provider) owns the Spawnly
  # delegation exchange by priority and enforces the frozen delegation policy;
  # this flag is what admits the token-exchange grant type for the client at all.
  # It does NOT widen scope — full_scope_allowed stays false and the provider
  # itself enforces scope-subset + the registry ceiling. The C# IdP allowed any
  # machine client to exchange, so enabling it on all six preserves parity.
  standard_token_exchange_enabled = true

  # Phase 4: enable the CIBA (Client-Initiated Backchannel Authentication) grant
  # on the consent-gated machine clients only. `oidc.ciba.grant.enabled` is the
  # Keycloak client attribute the CibaGrantType checks
  # (CibaConfig.isOIDCCIBAGrantEnabled); the provider passes it through
  # generically via `extra_config` (there is no first-class field in 5.8.0). The
  # realm's delivery mode is POLL (realm.tf), so no client_notification_endpoint
  # is needed. Enabling CIBA does NOT widen scope — the custom channel provider
  # binds the spawn edge from the verified SVID and the registry decides consent.
  extra_config = each.value.ciba ? { "oidc.ciba.grant.enabled" = "true" } : {}

  full_scope_allowed = false
}

resource "keycloak_openid_client_optional_scopes" "machine" {
  # Only declare the resource for clients that actually request extra scopes;
  # weather-monitor (openid only) needs no optional-scope attachment.
  for_each = {
    for k, v in local.machine_clients : k => v
    if length(v.optional_scopes) > 0
  }

  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.machine[each.key].id

  optional_scopes = [
    for s in each.value.optional_scopes : keycloak_openid_client_scope.api[s].name
  ]
}

# ===========================================================================
# DEFAULT client scopes — pinned so Terraform is AUTHORITATIVE over each
# client's default set.
#
# `keycloak_openid_client_optional_scopes` above only governs the OPTIONAL set.
# Without pinning the DEFAULT set, an out-of-band admin could attach a
# privileged scope (spawnly-roles / orchestrator:read / orchestrator:write) to a
# machine client as a DEFAULT scope — silently granting `role` or
# orchestrator:* on every token — and `terraform plan` would show NO drift
# (Terraform wasn't managing default scopes). Declaring these resources makes
# any such out-of-band addition show as drift and get reverted on the next
# apply, closing the hole in the clients.tf hardening invariant.
#
# The lists below are EXACTLY the Keycloak defaults observed live for a fresh
# realm (verified against a throwaway Keycloak), so pinning them strips nothing
# the clients need:
#   - standard-flow clients (dashboard, mobile): acr, basic, email, profile,
#     roles, web-origins.  (`roles` here is Keycloak's BUILT-IN realm_access.
#     roles scope, NOT our spawnly-roles — that stays OPTIONAL.)
#   - service-account clients (orchestrator, idp-consent, the six machine
#     clients): the same set PLUS service_account.
# None of these defaults include spawnly-roles or orchestrator:* — those remain
# optional and requestable only by the clients whose optional_scopes list them.
locals {
  default_scopes_standard        = ["acr", "basic", "email", "profile", "roles", "web-origins"]
  default_scopes_service_account = ["acr", "basic", "email", "profile", "roles", "service_account", "web-origins"]
}

resource "keycloak_openid_client_default_scopes" "dashboard" {
  realm_id       = keycloak_realm.spawnly.id
  client_id      = keycloak_openid_client.dashboard.id
  default_scopes = local.default_scopes_standard
}

resource "keycloak_openid_client_default_scopes" "mobile" {
  realm_id       = keycloak_realm.spawnly.id
  client_id      = keycloak_openid_client.mobile.id
  default_scopes = local.default_scopes_standard
}

resource "keycloak_openid_client_default_scopes" "orchestrator" {
  realm_id       = keycloak_realm.spawnly.id
  client_id      = keycloak_openid_client.orchestrator.id
  default_scopes = local.default_scopes_service_account
}

resource "keycloak_openid_client_default_scopes" "idp_consent" {
  realm_id       = keycloak_realm.spawnly.id
  client_id      = keycloak_openid_client.idp_consent.id
  default_scopes = local.default_scopes_service_account
}

resource "keycloak_openid_client_default_scopes" "org_admin" {
  realm_id       = keycloak_realm.spawnly.id
  client_id      = keycloak_openid_client.org_admin.id
  default_scopes = local.default_scopes_service_account
}

resource "keycloak_openid_client_default_scopes" "machine" {
  for_each = local.machine_clients

  realm_id       = keycloak_realm.spawnly.id
  client_id      = keycloak_openid_client.machine[each.key].id
  default_scopes = local.default_scopes_service_account
}

# ===========================================================================
# Phase 2 identity mapper — sub=user:<userId>, nested act, token_use.
#
# The custom "spawnly-identity-mapper" (SpawnlyIdentityMapper) reshapes a
# machine client_credentials token to the platform's delegated-identity shape,
# reading the registry-confirmed identity the attestation authenticator stashed
# in the request-scoped session. Attached to the SIX MACHINE CLIENTS ONLY —
# NEVER to dashboard/mobile (human) or orchestrator/idp-consent (control-plane).
#
# SECURITY: the mapper is a no-op unless the request is attestation-
# authenticated (a VerifiedAgent session attribute is present), so even if it
# were mistakenly attached elsewhere it could not override a human's sub. We
# still scope it to machine clients only, matching the C# validator that runs
# the sub/act injection exclusively on the agent client_credentials path.
# It emits NO `role` claim — role stays user-sourced (scopes.tf).
resource "keycloak_generic_protocol_mapper" "machine_identity" {
  for_each = local.machine_clients

  realm_id        = keycloak_realm.spawnly.id
  client_id       = keycloak_openid_client.machine[each.key].id
  name            = "spawnly-agent-identity"
  protocol        = "openid-connect"
  protocol_mapper = "spawnly-identity-mapper"

  # The mapper reads everything from the session attribute; no per-instance
  # config is required. Include it in both tokens (the access token is the
  # security-relevant one; the id_token is shaped identically for consistency).
  config = {
    "access.token.claim" = "true"
    "id.token.claim"     = "true"
  }
}
