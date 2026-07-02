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

  # offline_access mints a refresh token so the backend keeps its short-lived
  # orchestrator token fresh for the browser session (Config.cs AllowOfflineAccess).
  use_refresh_tokens = true

  full_scope_allowed = false

  consent_required = false
}

resource "keycloak_openid_client_optional_scopes" "dashboard" {
  realm_id  = keycloak_realm.spawnly.id
  client_id = keycloak_openid_client.dashboard.id

  # NOTE: only `profile` is a Keycloak DEFAULT default-client-scope that must
  # NOT be listed here (the provider errors "already attached as a default
  # scope"). `offline_access` is a default-OPTIONAL scope, so it MUST be listed
  # among the optional scopes below — `keycloak_openid_client_optional_scopes`
  # is authoritative and would otherwise DETACH it, killing the refresh token
  # (dashboard session dies at the realm idle timeout instead of lasting the 8h
  # session). See finding #3.
  optional_scopes = [
    "offline_access",
    keycloak_openid_client_scope.roles.name,
    keycloak_openid_client_scope.api["orchestrator:read"].name,
    keycloak_openid_client_scope.api["orchestrator:write"].name,
  ]
}

# sub = username. Keycloak's default `sub` is the user UUID, but the platform
# keys HUMAN identity on the username (orchestrator does
# strings.TrimPrefix(claims.User, "user:"); the sidecar sends
# login_hint="user:"+userID; IdentityServer's CibaUserValidator resolves it via
# FindByUsername). A UUID sub would split identity across the dual-issuer
# boundary and break consent, so we override `sub` = username for the human
# clients (dashboard + mobile), matching IdentityServer and the Phase 2
# sub-override direction. Machine/control-plane clients keep the default UUID
# sub (they're on IdentityServer in Phase 0 and carry no user identity anyway).
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

  # Only `profile` is a default default-scope (do not list it). `offline_access`
  # is a default-OPTIONAL scope and MUST be listed here or this authoritative
  # resource detaches it, killing the mobile long-lived session. See finding #3
  # and the dashboard note above.
  optional_scopes = [
    "offline_access",
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
  # clientId -> { lifespan (null = realm default), optional_scopes = [...] }
  machine_clients = {
    "weather-monitor" = {
      lifespan        = null
      optional_scopes = [] # openid only (openid is built-in default)
    }
    "chain-worker" = {
      lifespan = "120"
      optional_scopes = [
        "sample-api-a:read",
        "orchestrator:spawn",
      ]
    }
    "travel-planner" = {
      lifespan = null
      optional_scopes = [
        "orchestrator:spawn",
      ]
    }
    "flight-search" = {
      lifespan = "120"
      optional_scopes = [
        "flights:read",
        "orchestrator:spawn",
      ]
    }
    "hotel-search" = {
      lifespan = "120"
      optional_scopes = [
        "hotels:read",
        "orchestrator:spawn",
      ]
    }
    "fx-converter" = {
      lifespan = "120"
      optional_scopes = [
        "fx:read",
        "orchestrator:spawn",
      ]
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
