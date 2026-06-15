resource "spawnly_agent_template" "chain_worker" {
  agent_type      = "chain-worker"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true
  oauth_scopes    = ["openid", "sample-api-a:read"]

  meta {
    display_name = "Chain Worker"
    description  = "A long-lived worker that spawns one child of its own type (up to maxDepth) and calls the sample API on a loop. Demonstrates cascading real-time revocation across an agent chain, and CIBA spawn consent: the first spawned link prompts the user on the dashboard; later links auto-approve from the stored consent."
  }

  runtime_spec {
    image     = "agent-chain-worker:latest"
    lifecycle = "long-lived"

    env_defaults = {
      SCOPE            = "sample-api-a:read"
      WORK_INTERVAL_MS = "3000"
    }

    resources {
      cpu_limits    = "500m"
      memory_limits = "256Mi"
    }
  }

  authz_template {
    spicedb_relation {
      resource = "tenant:{{tenant_id}}"
      relation = "agent"
      subject  = "agent:{{agent_id}}"
    }
  }

  delegation {
    allowed_child_types = ["chain-worker"]
    max_depth           = 4

    child_policies = {
      "chain-worker" = {
        require_user_consent = true
        consent_ttl          = "720h"
      }
    }
  }
}
