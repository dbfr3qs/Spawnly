# ===========================================================================
# Client scopes — one per ApiScope in identityserver/Config.cs.
#
# All are OPTIONAL (clients request them explicitly) and never default, and no
# client has full_scope_allowed, so a client can only ever be granted a scope
# that is explicitly listed among ITS optional scopes (see clients.tf).
# ===========================================================================

locals {
  # Every ApiScope from Config.cs.ApiScopes.
  api_scopes = {
    "sample-api-a:read"  = "Read sample-api-a"
    "sample-api-a:write" = "Write sample-api-a"
    "sample-api-b:read"  = "Read sample-api-b"
    "sample-api-b:write" = "Write sample-api-b"
    "fx:read"            = "Convert currency (travel-tools)"
    "flights:read"       = "Search flights (travel-tools)"
    "hotels:read"        = "Search hotels (travel-tools)"
    "registry.consent"   = "Registry consent broker"
    "orchestrator:spawn" = "Spawn child agents via the orchestrator"
    "orchestrator:read"  = "Read via the orchestrator"
    "orchestrator:write" = "Act via the orchestrator"
  }

  # ApiResources scope->aud mapping (Config.cs.ApiResources): granting ANY
  # scope of a resource must emit the resource name into the token `aud`. We
  # reproduce this by attaching an audience mapper to EACH client scope that
  # belongs to the resource, keyed here scope -> resource (audience) name.
  scope_audience = {
    "sample-api-a:read"  = "sample-api-a"
    "sample-api-a:write" = "sample-api-a"
    "sample-api-b:read"  = "sample-api-b"
    "sample-api-b:write" = "sample-api-b"
    "fx:read"            = "travel-tools"
    "flights:read"       = "travel-tools"
    "hotels:read"        = "travel-tools"
    "registry.consent"   = "registry"
    "orchestrator:spawn" = "orchestrator"
    "orchestrator:read"  = "orchestrator"
    "orchestrator:write" = "orchestrator"
  }
}

resource "keycloak_openid_client_scope" "api" {
  for_each = local.api_scopes

  realm_id    = keycloak_realm.spawnly.id
  name        = each.key
  description = each.value

  # Keep the scope name in the token `scope` claim (resource servers read it).
  include_in_token_scope = true
}

# Audience mapper per scope: granting the scope adds the resource name to `aud`,
# reproducing Config.cs's ApiResource(name){ Scopes = {...} } behaviour.
resource "keycloak_openid_audience_protocol_mapper" "api" {
  for_each = local.scope_audience

  realm_id        = keycloak_realm.spawnly.id
  client_scope_id = keycloak_openid_client_scope.api[each.key].id
  name            = "aud-${each.value}"

  included_custom_audience = each.value

  add_to_id_token     = false
  add_to_access_token = true
}

# ===========================================================================
# `roles` client scope + flat `role` claim mapper.
#
# Mirrors Config.cs's IdentityResource("roles", ..., ["role"]). The claim is
# named exactly `role` (NOT realm_access.roles), single string value, and is
# emitted from the authenticated USER's realm-role membership.
#
# SECURITY INVARIANT (mirrors the comment in identityserver/Config.cs):
#   `role` must NEVER be a client-assertable claim. Here it is sourced ONLY
#   from the user's realm-role membership (keycloak_openid_user_realm_role_
#   protocol_mapper), never from a client attribute — so a client-credentials
#   token (which has no authenticated user) carries no `role` at all. This
#   `roles` scope is listed ONLY among the `dashboard` and `mobile` clients'
#   optional scopes (see clients.tf); because scopes are OPTIONAL and no client
#   has full_scope_allowed, a machine client requesting `roles` is granted
#   nothing. A forged role would defeat the admin gate — do not attach this
#   scope to any machine client.
# ===========================================================================
# NOTE: named `spawnly-roles`, NOT `roles`, because Keycloak ships a BUILT-IN
# default client scope literally named `roles` (it emits realm_access.roles /
# resource_access). Reusing that name collides at apply. We want a DISTINCT
# scope carrying our flat `role` claim, requestable only by dashboard/mobile.
resource "keycloak_openid_client_scope" "roles" {
  realm_id    = keycloak_realm.spawnly.id
  name        = "spawnly-roles"
  description = "Spawnly user role (flat `role` claim; admin gating)."

  # `role` is a claim, not an OAuth scope name we want echoed in `scope`.
  include_in_token_scope = false
}

resource "keycloak_openid_user_realm_role_protocol_mapper" "role" {
  realm_id        = keycloak_realm.spawnly.id
  client_scope_id = keycloak_openid_client_scope.roles.id
  name            = "role"

  # MULTIVALUED `role` claim (array of the user's realm roles). It MUST be
  # multivalued, not single-valued: Keycloak's built-in `default-roles-spawnly`
  # composite grants every user `offline_access` + `uma_authorization`, so a
  # single-valued realm-role mapper emits an ARBITRARY one of {offline_access,
  # uma_authorization, admin} and would non-deterministically DROP `admin` —
  # silently breaking the admin gate. (This regressed once users.tf stopped
  # stripping default-roles-spawnly, finding #4; the scope role-scope mapping
  # below does NOT filter this custom claim mapper's output, verified live.)
  # Both consumers key on MEMBERSHIP of "admin" (tokenvalidator.HasRole /
  # dashboard auth.go roleIsAdmin, which accept a string OR an array), so an
  # array carrying admin gates correctly and fail-closed; the extra built-in
  # roles are harmless.
  claim_name       = "role"
  claim_value_type = "String"
  multivalued      = true

  # Lands in BOTH tokens: the dashboard/mobile read it from the id_token, the
  # orchestrator reads it from the access token (aud=orchestrator).
  add_to_id_token     = true
  add_to_access_token = true
  add_to_userinfo     = true
}

# The realm-role protocol mapper only emits roles that are within the token's
# EFFECTIVE scope. Every client has full_scope_allowed = false (clients.tf
# hardening), so without an explicit scope→role mapping the `admin` role is
# filtered out and `role` never appears — verified against a live Keycloak. We
# add the `admin` realm role to the `spawnly-roles` client scope's role-scope
# mappings, so a user who is a member of `admin` and whose client requested
# `spawnly-roles` (only dashboard/mobile can) gets role=admin. This does NOT
# widen the invariant: `spawnly-roles` is still requestable only by
# dashboard/mobile, and a client-credentials token has no user to be an `admin`
# member, so machine tokens still carry no `role`.
resource "keycloak_generic_role_mapper" "spawnly_roles_admin" {
  realm_id        = keycloak_realm.spawnly.id
  client_scope_id = keycloak_openid_client_scope.roles.id
  role_id         = keycloak_role.admin.id
}
