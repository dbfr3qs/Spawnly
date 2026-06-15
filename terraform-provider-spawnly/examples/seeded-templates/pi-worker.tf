resource "spawnly_agent_template" "pi_worker" {
  agent_type      = "pi-worker"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = false

  meta {
    display_name = "Pi Coding Agent"
    description  = "Global (tenant-less) long-lived coding agent built on the Pi harness — chat it a coding task and watch its tool/model timeline, or ask it to call the protected Sample API with its Spawnly identity"
  }

  runtime_spec {
    image         = "agent-pi-worker:latest"
    lifecycle     = "long-lived"
    supports_chat = true

    env_defaults = {
      LOG_LEVEL      = "info"
      AI_MODEL       = "openai/gpt-4o"
      SAMPLE_API_URL = "http://sample-api-global"
      SCOPE          = "sample-api-a:read"
    }

    resources {
      cpu_limits    = "1"
      memory_limits = "512Mi"
    }
  }
}
