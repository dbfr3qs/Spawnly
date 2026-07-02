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

  # ── CIBA policy (Phase 4) ────────────────────────────────────────────────
  # The realm's CIBA (Client-Initiated Backchannel Authentication) policy. These
  # are Keycloak server-side realm attributes passed through the provider's
  # generic `attributes` map (the keycloak/keycloak 5.8.0 provider has no
  # first-class CIBA policy block). Values match identityserver/Config.cs
  # (CibaLifetime=300s, PollingInterval=5s):
  #   - POLL delivery mode: the sidecar polls the token endpoint for the
  #     auth_req_id (no ping/push callback to the agent) — matching the C#/sidecar
  #     driver (cmd/agent-sidecar/ciba.go polls with grant_type=…:ciba).
  #   - cibaExpiresIn=300: a backchannel request lives 300s (the consent window).
  #   - cibaInterval=5: minimum 5s between token polls.
  #   - cibaAuthRequestedUserHint=login_hint: the sidecar sends the spawning
  #     user as login_hint="user:<id>" (resolved to a Keycloak user, then
  #     cross-checked against the agent's registry userId by the channel provider).
  attributes = {
    cibaBackchannelTokenDeliveryMode = "poll"
    cibaExpiresIn                    = "300"
    cibaInterval                     = "5"
    cibaAuthRequestedUserHint        = "login_hint"
  }
}
