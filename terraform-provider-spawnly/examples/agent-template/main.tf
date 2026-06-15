terraform {
  required_providers {
    spawnly = {
      source = "registry.terraform.io/spawnly/spawnly"
    }
  }
}

provider "spawnly" {
  endpoint = "http://localhost:18080" # port-forwarded registry
  # token sourced from SPAWNLY_TOKEN (the bootstrap-generated control-plane secret)
}

resource "spawnly_agent_template" "demo" {
  agent_type      = "tf-demo-worker"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = false
  oauth_scopes    = ["sample-api-a:write"]

  meta {
    display_name = "TF Demo Worker"
    description  = "Created by the Spawnly Terraform provider"
  }

  runtime_spec {
    image         = "agent-go-worker:latest"
    lifecycle     = "short-lived"
    supports_chat = false

    env_defaults = {
      SAMPLE_API_URL = "http://sample-api-global"
      LOG_LEVEL      = "info"
    }

    resources {
      cpu_limits    = "500m"
      memory_limits = "256Mi"
    }
  }
}
