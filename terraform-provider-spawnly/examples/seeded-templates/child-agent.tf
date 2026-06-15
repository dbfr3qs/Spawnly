resource "spawnly_agent_template" "child_agent" {
  agent_type      = "child-agent"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true

  meta {
    display_name = "Child Agent"
    description  = "Long-lived A2A server that generates a random string on request"
  }

  runtime_spec {
    image     = "agent-child-agent:latest"
    lifecycle = "long-lived"

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
