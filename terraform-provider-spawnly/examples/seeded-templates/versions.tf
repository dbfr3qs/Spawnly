# Terraform representation of the agent templates that `make bootstrap` seeds via
# scripts/seed.sh (agents/*/template.json). This is a validation/dogfood config:
# import the already-seeded templates and confirm `terraform plan` is clean, i.e.
# the HCL faithfully represents each template and the provider round-trips it.
#
# NOTE: seed.sh remains the operational bootstrap path. The registry store is
# in-memory and resets on restart, so Terraform is not (yet) the source of truth
# for templates — that awaits a persistent registry store.
terraform {
  required_providers {
    spawnly = {
      source = "registry.terraform.io/spawnly/spawnly"
    }
  }
}

provider "spawnly" {
  endpoint = "http://localhost:18080" # port-forwarded registry
  # token from SPAWNLY_TOKEN
}
