# ===========================================================================
# Custom registration flow: a copy of Keycloak's built-in registration flow with
# the "spawnly-user-cap" form action added as a REQUIRED execution
# (signup-lifecycle Phase 2, MAX_USERS global ceiling).
#
# WHY A NEW FLOW: Keycloak's built-in "registration" flow rejects added
# executions ("illegal to add execution to a built in flow"), so — exactly like
# the client-auth flow in authenticator.tf — we declare our OWN top-level
# registration flow, replicate the built-in registration form actions
# (user creation + password) as REQUIRED executions, add spawnly-user-cap, and
# BIND it as the realm's registration flow (see keycloak_authentication_bindings
# in authenticator.tf).
#
# WHY IT ONLY GATES SELF-SERVE SIGNUP: this flow runs on the register PAGE only.
# Admin-API user creation (the orgdirectory adapter's createUser, used by org
# onboarding / membership) does not exercise the registration flow, so it bypasses
# the cap naturally — the "admin provisions beyond the cap" behaviour in the spec.
#
# ORDERING NOTE: Keycloak runs every form action's validate() before any
# success(); the user is only persisted in registration-user-creation's success().
# So spawnly-user-cap's validate() reads the PRE-creation user count and a
# rejection stops the flow before any account is created — no orphan user.

resource "keycloak_authentication_flow" "registration_spawnly" {
  realm_id    = keycloak_realm.spawnly.id
  alias       = "registration-spawnly"
  description = "Registration flow with the Spawnly global user-cap form action"
  # A top-level registration flow (basic-flow), matching the built-in "registration".
  provider_id = "basic-flow"
}

# The registration FORM subflow that hosts the page's form actions. It MUST be
# rendered by the `registration-page-form` authenticator — that is the form
# provider that actually draws the register page (register.ftl: profile fields,
# password, submit). Without it the subflow is a bare form-flow with no page
# renderer and the register form never renders (the #username input is absent).
# This mirrors the built-in "registration form" subflow, whose bound authenticator
# is exactly `registration-page-form`. `provider_id = form-flow` is the flow TYPE;
# `authenticator = registration-page-form` is the page renderer bound to it.
resource "keycloak_authentication_subflow" "registration_form_spawnly" {
  realm_id          = keycloak_realm.spawnly.id
  alias             = "registration-form-spawnly"
  parent_flow_alias = keycloak_authentication_flow.registration_spawnly.alias
  provider_id       = "form-flow"
  authenticator     = "registration-page-form"
  requirement       = "REQUIRED"
}

# Built-in form actions, replicated so the register page still creates the user
# and sets their password. REQUIRED, matching the stock registration flow.
resource "keycloak_authentication_execution" "registration_user_creation" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_subflow.registration_form_spawnly.alias
  authenticator     = "registration-user-creation"
  requirement       = "REQUIRED"
}

resource "keycloak_authentication_execution" "registration_password_action" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_subflow.registration_form_spawnly.alias
  authenticator     = "registration-password-action"
  requirement       = "REQUIRED"
  depends_on        = [keycloak_authentication_execution.registration_user_creation]
}

# The Spawnly global user cap. Provider id must match
# UserCapFormAction.PROVIDER_ID. REQUIRED so it actually gates signups; the cap
# itself is armed by the MAX_USERS env on the Keycloak container (unset = off).
resource "keycloak_authentication_execution" "spawnly_user_cap" {
  realm_id          = keycloak_realm.spawnly.id
  parent_flow_alias = keycloak_authentication_subflow.registration_form_spawnly.alias
  authenticator     = "spawnly-user-cap"
  requirement       = "REQUIRED"
  depends_on        = [keycloak_authentication_execution.registration_password_action]
}
