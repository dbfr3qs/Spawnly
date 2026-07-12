# ===========================================================================
# Custom client-authentication flow: adds the "spawnly-attestation" executor
# (Phase 1) alongside the built-in client authenticators.
#
# WHY A NEW FLOW: Keycloak's client-authentication FLOW iterates its EXECUTIONS
# and, for each, runs only the authenticator whose getId() matches the client's
# client_authenticator_type. The custom authenticator must therefore be an
# execution in the bound client-auth flow. The built-in "clients" flow rejects
# added executions ("illegal to add execution to a built in flow"), so we
# declare our OWN flow, replicate the built-in client authenticators as
# ALTERNATIVE executions (so human/control-plane clients keep client-secret,
# and standard client-jwt/x509 still work), add spawnly-attestation, and BIND
# it as the realm's client-authentication flow.
#
# ALTERNATIVE across all: exactly the authenticator matching each client's
# client_authenticator_type authenticates it; the others no-op for that client.
resource "keycloak_authentication_flow" "clients_spawnly" {
  realm_id    = keycloak_realm.spawnly.id
  alias       = "clients-spawnly"
  description = "Client auth flow with the Spawnly attestation authenticator"
  # A client-authentication flow, not a browser/login flow.
  provider_id = "client-flow"
}

# Built-in client authenticators, replicated so nothing regresses for the
# human/control-plane clients (client-secret) or standard JWT/x509 clients.
resource "keycloak_authentication_execution" "client_secret" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_flow.clients_spawnly.alias
  authenticator     = "client-secret"
  requirement       = "ALTERNATIVE"
}

resource "keycloak_authentication_execution" "client_jwt" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_flow.clients_spawnly.alias
  authenticator     = "client-jwt"
  requirement       = "ALTERNATIVE"
  depends_on        = [keycloak_authentication_execution.client_secret]
}

resource "keycloak_authentication_execution" "client_secret_jwt" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_flow.clients_spawnly.alias
  authenticator     = "client-secret-jwt"
  requirement       = "ALTERNATIVE"
  depends_on        = [keycloak_authentication_execution.client_jwt]
}

resource "keycloak_authentication_execution" "client_x509" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_flow.clients_spawnly.alias
  authenticator     = "client-x509"
  requirement       = "ALTERNATIVE"
  depends_on        = [keycloak_authentication_execution.client_secret_jwt]
}

# The Spawnly attestation authenticator. Provider id must match
# SpawnlyAttestationAuthenticator.PROVIDER_ID.
resource "keycloak_authentication_execution" "spawnly_attestation" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_flow.clients_spawnly.alias
  authenticator     = "spawnly-attestation"
  requirement       = "ALTERNATIVE"
  depends_on        = [keycloak_authentication_execution.client_x509]
}

# Bind our custom flows as the realm's client-authentication and registration
# flows. There is exactly ONE authentication-bindings resource per realm (it
# manages ALL of the realm's flow bindings together), so the registration-flow
# binding from registration-flow.tf is set HERE rather than in a second bindings
# resource — two of them would fight over the realm's binding config on every
# apply. Any binding left unset falls back to Keycloak's built-in flow of that
# name (e.g. browser/reset-credentials stay stock).
resource "keycloak_authentication_bindings" "spawnly" {
  realm_id                   = keycloak_realm.spawnly.id
  client_authentication_flow = keycloak_authentication_flow.clients_spawnly.alias
  registration_flow          = keycloak_authentication_flow.registration_spawnly.alias
  depends_on = [
    keycloak_authentication_execution.spawnly_attestation,
    keycloak_authentication_execution.spawnly_user_cap,
  ]
}
