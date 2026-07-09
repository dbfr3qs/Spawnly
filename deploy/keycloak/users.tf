# ===========================================================================
# Users — the demo persona set for the multi-tenancy / visibility matrix.
#
#   root  = PLATFORM master admin: holds the realm `admin` role, member of NO
#           org (the cross-org/Global persona).
#   alice = acme ORG admin only (SpiceDB org-admin tuple via reseed-orgs.sh) —
#           she does NOT hold the realm `admin` role.
#   viewer/bob/carol/nomad = non-admin org members (see each block).
#
# Passwords come from TF variables; nothing is committed. Fail-closed: each
# user is created ONLY when its password var is non-empty (count = 0 otherwise),
# so a production tfvars can omit the viewer entirely — matching today's
# "absent password => no user" posture.
# ===========================================================================

# Realm role `admin` — the PLATFORM master admin role. Held ONLY by root; NO
# demo org admin (alice/bob) holds it. Org-admin authority is a SpiceDB tuple,
# a separate concept from this realm role.
resource "keycloak_role" "admin" {
  realm_id    = keycloak_realm.spawnly.id
  name        = "admin"
  description = "Spawnly administrator (gates the admin UI and orchestrator template management)."
}

# ---- root: platform master admin (realm `admin`, member of NO org) ---------
# The dedicated platform-operator persona: holds the realm `admin` role and is
# NEVER added to any org (reseed-orgs.sh does not touch root — same posture as
# nomad), so its token stays org-less. This is the ONLY user with the realm
# `admin` role and thus the only cross-org/Global persona. Same fail-closed
# count pattern as every other seeded user.
resource "keycloak_user" "root" {
  count = var.root_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "root"
  enabled  = true

  email          = "root@example.com"
  email_verified = true
  first_name     = "Root"
  last_name      = "Admin"

  initial_password {
    value     = var.root_password
    temporary = false
  }
}

resource "keycloak_user_roles" "root_admin" {
  count = var.root_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  user_id  = keycloak_user.root[0].id

  role_ids = [keycloak_role.admin.id]

  # NON-EXHAUSTIVE: only ensure `admin` is present, do not manage the user's
  # full role set. Exhaustive (the default) would STRIP the built-in
  # `default-roles-spawnly` composite role that carries `offline_access` /
  # `uma_authorization`, which would silently kill offline_access tokens for
  # root. See finding #4.
  exhaustive = false
}

# ---- alice: acme org admin (NO realm role) ---------------------------------
# alice's admin authority is scoped to acme via the SpiceDB org-admin tuple
# written by reseed-orgs.sh (`create_org acme "Acme" alice`), NOT a realm role —
# she is deliberately NOT granted `keycloak_role.admin`, so she has no cross-org
# Global view. Same fail-closed count pattern as the other users.
resource "keycloak_user" "alice" {
  count = var.alice_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "alice"
  enabled  = true

  email          = "alice@example.com"
  email_verified = true
  first_name     = "Alice"
  last_name      = "Acme"

  initial_password {
    value     = var.alice_password
    temporary = false
  }
  # No realm admin role — alice's authority over acme is a SpiceDB org-admin
  # tuple (reseed-orgs.sh), not the realm `admin` role. Only root is realm admin.
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

# ---- carol: non-admin user, member of TWO orgs (acme + globex) -------------
# Exercises silent cross-subdomain SSO (org-subdomains e2e case b): carol is a
# plain member of BOTH acme and globex (SpiceDB tuples via reseed-orgs.sh, NOT a
# realm role), so logging in at one org's subdomain and visiting the other's must
# complete silently (prompt=none on the shared apex SSO session). She is added to
# both orgs AFTER their admins/members are seeded, so she does not perturb the
# single-org isolation matrix (orgs.spec.ts / membership.spec.ts assert on the
# users they name, never a member count). Same fail-closed count pattern.
resource "keycloak_user" "carol" {
  count = var.carol_password != "" ? 1 : 0

  realm_id = keycloak_realm.spawnly.id
  username = "carol"
  enabled  = true

  email          = "carol@example.com"
  email_verified = true
  first_name     = "Carol"
  last_name      = "Crossorg"

  initial_password {
    value     = var.carol_password
    temporary = false
  }
  # No realm admin role — carol is an ordinary member of two orgs (SpiceDB tuples).
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
