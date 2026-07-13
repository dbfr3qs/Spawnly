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
  # dashboard's no-organization screen -> POST /api/signup/org. The registration
  # page inherits the `spawnly` login theme (CSS-only child of keycloak.v2, which
  # ships register.ftl), so it renders in the same palette as the login page.
  #
  # Email verification (signup-lifecycle Phase 1): ON exactly when SMTP is
  # configured (var.smtp_host non-empty → the smtp_server block below is emitted).
  # With no SMTP (empty host) it stays OFF, byte-identical to the closed-beta
  # behaviour — signup is not gated on mail delivery when there is no mail server.
  # Admin-API-created users are pinned emailVerified=true in the adapter, so
  # verification applies to the self-serve registration path ONLY.
  registration_allowed = true
  verify_email         = var.smtp_host != ""

  # SMTP server for realm email (verification, etc.). Emitted only when an SMTP
  # host is configured; the nested auth block only when a username is set (the
  # kind Mailpit sink needs no auth). Prod values come from AWS SES.
  dynamic "smtp_server" {
    for_each = var.smtp_host != "" ? [1] : []
    content {
      host = var.smtp_host
      port = var.smtp_port
      from = var.smtp_from

      # STARTTLS is required exactly when we authenticate: AWS SES submission
      # (:587) rejects AUTH before a STARTTLS upgrade, so an authed relay must
      # negotiate it. The kind Mailpit sink is authless plaintext (smtp_user
      # empty) and must NOT request STARTTLS. Keying starttls off smtp_user thus
      # turns it on for SES and leaves the kind realm byte-identical — no extra
      # var to thread through bootstrap.sh / apply-realm.sh / the infra deploy.
      starttls = var.smtp_user != ""

      dynamic "auth" {
        for_each = var.smtp_user != "" ? [1] : []
        content {
          username = var.smtp_user
          password = var.smtp_password
        }
      }
    }
  }

  # Brute-force protection (signup-lifecycle Phase 1) — unconditional, using
  # Keycloak's stock defaults (temporary per-user lockout after 30 consecutive
  # failures: 1-min wait increment, 15-min max wait, 12-hour failure reset). This
  # hardens BOTH the self-serve registration path and every existing login. Set
  # explicitly rather than relying on the provider's zero-value block defaults,
  # which would otherwise drive the thresholds to 0 and lock accounts far too
  # aggressively (breaking normal logins). Single-replica safe (--cache=local).
  security_defenses {
    brute_force_detection {
      permanent_lockout                = false
      max_login_failures               = 30
      wait_increment_seconds           = 60
      quick_login_check_milli_seconds  = 1000
      minimum_quick_login_wait_seconds = 60
      max_failure_wait_seconds         = 900
      failure_reset_time_seconds       = 43200
    }
  }

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

  # Guard: Keycloak's SMTP config requires a From address, so if an SMTP host is
  # configured (which also flips verify_email on) a from address MUST be set —
  # otherwise the realm would apply a broken mail config and verification mail
  # would silently fail to send. bootstrap.sh defaults smtp_from on kind, but a
  # prod/env override that sets host without from is caught here at plan time
  # instead of at first send.
  lifecycle {
    precondition {
      condition     = var.smtp_host == "" || var.smtp_from != ""
      error_message = "smtp_from must be set when smtp_host is configured (Keycloak requires a From address for realm email)."
    }
  }
}
