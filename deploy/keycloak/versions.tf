terraform {
  required_version = ">= 1.5.0"

  required_providers {
    keycloak = {
      source = "keycloak/keycloak"
      # Pinned to the latest 5.x patch at authoring time. The realm schema
      # (client scopes, audience/role protocol mappers, CIBA client attrs)
      # used below is stable across 5.x.
      version = "5.8.0"
    }
  }
}
