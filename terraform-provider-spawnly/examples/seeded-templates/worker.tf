# NOTE: the go-worker agent registers its type as "worker".
resource "spawnly_agent_template" "worker" {
  agent_type      = "worker"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true

  meta {
    display_name = "Generic Worker"
    description  = "Calls the sample API and exits"
  }

  runtime_spec {
    image = "agent-go-worker:latest"

    env_defaults = {
      LOG_LEVEL   = "info"
      MAX_RETRIES = "3"
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
}
