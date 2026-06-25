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

# A tenanted coordinator template that may spawn children, delegate a scope to
# them, and gate one child type behind CIBA user consent. Exercises the richer
# corners of the schema: authz_template, delegation (allowed_child_types +
# grantable_scopes + max_depth + a child_policies map entry) and oauth_scopes.
resource "spawnly_agent_template" "coordinator" {
  agent_type      = "tf-demo-coordinator"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true # authz relations below reference {{tenant_id}}
  oauth_scopes    = ["sample-api-a:write", "sample-api-b:read"]

  meta {
    display_name = "TF Demo Coordinator"
    description  = "Coordinator that spawns gated children, created via Terraform"
  }

  runtime_spec {
    image         = "agent-chain-worker:latest"
    lifecycle     = "long-lived"
    supports_chat = true

    env_defaults = {
      SAMPLE_API_URL = "http://sample-api-a"
      LOG_LEVEL      = "info"
    }

    resources {
      cpu_limits    = "500m"
      memory_limits = "512Mi"
    }
  }

  # SpiceDB relations written when an agent of this type registers. Placeholders
  # are substituted at registration time.
  authz_template {
    spicedb_relation {
      resource = "tenant:{{tenant_id}}"
      relation = "agent"
      subject  = "agent:{{agent_id}}"
    }
  }

  # What this type may spawn and how those spawns are gated.
  delegation {
    allowed_child_types = ["tf-demo-worker", "tf-demo-reporter"]
    grantable_scopes    = ["sample-api-a:write"]
    max_depth           = 2

    # Per-child-type gating, keyed by child agent type. Only the worker spawn is
    # gated behind user consent here.
    child_policies = {
      "tf-demo-worker" = {
        require_user_consent = true
        consent_ttl          = "720h" # consent lasts 30 days
      }
    }
  }
}
