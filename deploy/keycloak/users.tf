# ===========================================================================
# Users — mirror identityserver/TestUsers.cs (alice = admin, viewer = no role).
#
# Passwords come from TF variables; nothing is committed. Fail-closed: each
# user is created ONLY when its password var is non-empty (count = 0 otherwise),
# so a production tfvars can omit the viewer entirely — matching today's
# "absent password => no user" posture.
# ===========================================================================

# Realm role `admin`; alice is a member, viewer is not.
resource "keycloak_role" "admin" {
  realm_id    = keycloak_realm.spawnly.id
  name        = "admin"
  description = "Spawnly administrator (gates the admin UI and orchestrator template management)."
}

# ---- alice: admin user -----------------------------------------------------
resource "keycloak_user" "alice" {
  count = var.alice_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "alice"
  enabled  = true

  email          = "alice@example.com"
  email_verified = true
  first_name     = "Alice"
  last_name      = "Admin"

  initial_password {
    value     = var.alice_password
    temporary = false
  }
}

resource "keycloak_user_roles" "alice_admin" {
  count = var.alice_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  user_id  = keycloak_user.alice[0].id

  role_ids = [keycloak_role.admin.id]

  # NON-EXHAUSTIVE: only ensure `admin` is present, do not manage the user's
  # full role set. Exhaustive (the default) would STRIP the built-in
  # `default-roles-spawnly` composite role that carries `offline_access` /
  # `uma_authorization`, which would silently kill offline_access tokens for
  # alice. See finding #4.
  exhaustive = false
}

# ---- viewer: non-admin user ------------------------------------------------
resource "keycloak_user" "viewer" {
  count = var.viewer_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "viewer"
  enabled  = true

  email          = "viewer@example.com"
  email_verified = true
  first_name     = "Vic"
  last_name      = "Viewer"

  initial_password {
    value     = var.viewer_password
    temporary = false
  }
  # No admin role — the `role` claim is simply absent for this user.
}

# ---- bob: non-admin user, second org (globex) ------------------------------
# Exercises cross-org isolation: bob is the org admin of `globex` (via the
# Phase-3 onboarding API, SpiceDB tuple — NOT a realm role), so he must see none
# of alice's `acme` agents. Same fail-closed count pattern as viewer.
resource "keycloak_user" "bob" {
  count = var.bob_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "bob"
  enabled  = true

  email          = "bob@example.com"
  email_verified = true
  first_name     = "Bob"
  last_name      = "Builder"

  initial_password {
    value     = var.bob_password
    temporary = false
  }
  # No realm admin role — bob's org-admin authority over globex is a SpiceDB
  # tuple written by the onboarding API, not the realm `admin` role.
}

# ---- nomad: non-admin user in NO organization ------------------------------
# Deliberately never added to any org (reseed-orgs.sh does not touch nomad), so a
# fresh login lands on the dashboard's "no organization" screen — the Phase-5 e2e
# proof of that state. Non-admin (no realm role) with a usable password, since a
# user created via the org onboarding API carries a required-password-reset action
# and no usable password (so it can't complete a browser login); a seeded org-less
# user is the testable equivalent.
resource "keycloak_user" "nomad" {
  count = var.nomad_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "nomad"
  enabled  = true

  email          = "nomad@example.com"
  email_verified = true
  first_name     = "Nomad"
  last_name      = "Unaffiliated"

  initial_password {
    value     = var.nomad_password
    temporary = false
  }
}
