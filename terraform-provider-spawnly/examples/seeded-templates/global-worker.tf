resource "spawnly_agent_template" "global_worker" {
  agent_type = "global-worker"
  version    = "1.0.0"
  status     = "active"

  meta {
    display_name = "Global Worker"
    description  = "Tenant-less worker that calls a tenant-agnostic sample API and exits"
  }

  runtime_spec {
    image     = "agent-go-worker:latest"
    lifecycle = "short-lived"

    env_defaults = {
      SAMPLE_API_URL = "http://sample-api-global"
      SCOPE          = "sample-api-a:write"
      TASK           = "hello from a tenant-less agent"
      LOG_LEVEL      = "info"
    }

    resources {
      cpu_limits    = "500m"
      memory_limits = "256Mi"
    }
  }
}
