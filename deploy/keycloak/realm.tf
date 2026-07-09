# The `spawnly` realm. In-cluster the issuer is EXACTLY
# http://keycloak:8080/realms/spawnly. Signing keys are Keycloak-managed
# (generated per realm at runtime) — never committed.
resource "keycloak_realm" "spawnly" {
  realm   = "spawnly"
  enabled = true

  display_name = "Spawnly"

  # Multi-tenancy (organizations) — turn on Keycloak Organizations for the single
  # `spawnly` realm. GA in the pinned 26.4 image, so no server feature flag is
  # needed; this realm switch is all that's required. Enabling it makes Keycloak
  # provision the built-in `organization` client scope (attached as an OPTIONAL
  # scope to the human clients below), whose membership mapper stamps the
  # `organization` claim when a login requests `organization` / `organization:<alias>`.
  # Additive: no org exists until the Phase-3 onboarding API creates one, and no
  # resource server reads the claim yet. The tenant primitive is untouched.
  organizations_enabled = true

  # Self-serve signup (multi-tenancy Phase C / Phase 7). Turn on the realm's
  # registration form so a brand-new user can create an account from the login
  # page; they then create their own organization (becoming its admin) via the
  # dashboard's no-organization screen -> POST /api/signup/org. Email
  # verification stays OFF: there is no SMTP configured for the closed beta, and
  # the no-invitations decision means signup is not gated on email delivery.
  # Abuse controls are an explicit non-goal for the beta (see spec phase C); the
  # only signup guardrails are the reserved-slug list and one-org-per-user limit
  # enforced server-side in the registry. The registration page inherits the
  # `spawnly` login theme (CSS-only child of keycloak.v2, which ships
  # register.ftl), so it renders in the same palette as the login page.
  registration_allowed = true
  verify_email         = false

  # Spawnly-branded login pages. The `spawnly` theme directory ships in the
  # Keycloak image (root Dockerfile copies keycloak/themes/spawnly into
  # /opt/keycloak/themes/) — it MUST exist there or Keycloak falls back/errors
  # on login, so this attribute and that COPY change ship in the same commit.
  login_theme = "spawnly"

  # Refresh-token rotation, one-time use. The mobile client relies on
  # rotation (Config.cs: RefreshTokenUsage.OneTimeOnly); enforcing it at the
  # realm keeps the posture even for the dashboard's refresh token.
  revoke_refresh_token    = true
  refresh_token_max_reuse = 0

  # ── SSO session windows ────────────────────────────────────────────────────
  # The dashboard deliberately does NOT request offline_access: since Keycloak
  # 26.1 (keycloak/keycloak#32650) an initial login with offline_access DELETES
  # the online SSO session, which broke the silent (prompt=none) org switch —
  # every switch fell back to a full username/password prompt. The dashboard's
  # long-lived session therefore rides ONLINE refresh tokens, whose lifetime is
  # bounded by these realm windows (Keycloak defaults: 30m idle / 10h max):
  #   - idle 8h  = the dashboard's own sliding session window (cmd/dashboard
  #     sessionTTL), so a user who walks away and returns within the dashboard
  #     session can still refresh without a new login;
  #   - max 24h = a hard daily cap on any single SSO session (active use past
  #     24h re-authenticates), comfortably above the 8h sliding window.
  sso_session_idle_timeout = "8h"
  sso_session_max_lifespan = "24h"

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
