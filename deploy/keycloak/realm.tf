# The `spawnly` realm. In-cluster the issuer is EXACTLY
# http://keycloak:8080/realms/spawnly. Signing keys are Keycloak-managed
# (generated per realm at runtime) — never committed.
resource "keycloak_realm" "spawnly" {
  realm   = "spawnly"
  enabled = true

  display_name = "Spawnly"

  # Refresh-token rotation, one-time use. The mobile client relies on
  # rotation (Config.cs: RefreshTokenUsage.OneTimeOnly); enforcing it at the
  # realm keeps the posture even for the dashboard's offline_access token.
  revoke_refresh_token    = true
  refresh_token_max_reuse = 0
}
