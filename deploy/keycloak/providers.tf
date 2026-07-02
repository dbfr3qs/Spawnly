# The provider authenticates to Keycloak's `master` realm via the password
# grant on the built-in `admin-cli` client (no client secret). The URL and
# admin credentials come from variables — never hardcoded. bootstrap.sh
# port-forwards Keycloak and feeds the generated admin creds as TF vars.
provider "keycloak" {
  client_id = "admin-cli"
  username  = var.keycloak_admin_username
  password  = var.keycloak_admin_password
  url       = var.keycloak_url
  # `admin-cli` lives in the master realm.
  realm = "master"
}
