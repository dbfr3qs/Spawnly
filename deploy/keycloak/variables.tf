# ---------------------------------------------------------------------------
# Provider / connection
# ---------------------------------------------------------------------------

variable "keycloak_url" {
  description = "Base URL of the Keycloak instance (e.g. a local port-forward). NOT the realm issuer."
  type        = string
  default     = "http://localhost:8080"
}

variable "keycloak_admin_username" {
  description = "Keycloak master-realm admin username (password grant on admin-cli)."
  type        = string
  sensitive   = true
}

variable "keycloak_admin_password" {
  description = "Keycloak master-realm admin password."
  type        = string
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Human login
# ---------------------------------------------------------------------------

variable "dashboard_origin" {
  description = "Browser-facing origin of the dashboard; used to build the redirect/post-logout URIs. Mirrors Config.cs DashboardOrigin."
  type        = string
  default     = "http://localhost:8090"
}

variable "dashboard_client_secret" {
  description = "Client secret for the confidential `dashboard` client."
  type        = string
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Control-plane clients (client-credentials)
# ---------------------------------------------------------------------------

variable "orchestrator_client_secret" {
  description = "Client secret for the `orchestrator` control-plane client."
  type        = string
  sensitive   = true
}

variable "idp_consent_client_secret" {
  description = "Client secret for the `idp-consent` control-plane client."
  type        = string
  sensitive   = true
}

variable "org_admin_client_secret" {
  description = "Client secret for the `org-admin` control-plane client (registry org directory). Bootstrap-generated; never committed."
  type        = string
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Machine clients
# ---------------------------------------------------------------------------

variable "machine_client_secret" {
  description = <<-EOT
    Placeholder client secret shared by the six machine clients for Phase 0.
    Mirrors Config.cs's Secret("placeholder"). In Phase 1 these clients switch
    to the custom attestation (SPIFFE JWT-SVID) client authenticator and this
    secret goes away.
  EOT
  type        = string
  sensitive   = true
}

# ---------------------------------------------------------------------------
# Users — passwords come from TF vars; nothing committed.
#
# Fail-closed posture (mirrors today's "absent password => no user"): each
# user is created only when its password var is non-empty. A production tfvars
# can omit the viewer entirely by leaving viewer_password = "".
# ---------------------------------------------------------------------------

variable "root_password" {
  description = "Initial password for the platform master admin `root` (holds the realm `admin` role, member of no org). Empty => root not created."
  type        = string
  sensitive   = true
  default     = ""
}

variable "alice_password" {
  description = "Initial password for the acme org admin `alice` (SpiceDB org-admin tuple, NOT a realm role). Empty => alice not created."
  type        = string
  sensitive   = true
  default     = ""
}

variable "viewer_password" {
  description = "Initial password for the non-admin user `viewer`. Empty => viewer not created."
  type        = string
  sensitive   = true
  default     = ""
}

variable "bob_password" {
  description = "Initial password for the non-admin user `bob` (org admin of the demo `globex` org via SpiceDB, not a realm role). Empty => bob not created."
  type        = string
  sensitive   = true
  default     = ""
}

variable "carol_password" {
  description = "Initial password for the non-admin user `carol`, a member of TWO orgs (acme + globex) that drives the org-subdomains silent cross-subdomain SSO e2e. Empty => carol not created."
  type        = string
  sensitive   = true
  default     = ""
}

variable "nomad_password" {
  description = "Initial password for the non-admin user `nomad`, a member of NO organization (drives the dashboard 'no organization' screen in e2e). Empty => nomad not created."
  type        = string
  sensitive   = true
  default     = ""
}
