resource "spawnly_agent_template" "weather_monitor" {
  agent_type      = "weather-monitor"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = false

  meta {
    display_name = "Weather Monitor"
    description  = "Global (tenant-less) long-lived agent you can chat with to check the weather anywhere"
  }

  runtime_spec {
    image         = "agent-weather-monitor:latest"
    lifecycle     = "long-lived"
    supports_chat = true

    env_defaults = {
      LOG_LEVEL = "info"
    }

    resources {
      cpu_limits    = "500m"
      memory_limits = "256Mi"
    }
  }
}
