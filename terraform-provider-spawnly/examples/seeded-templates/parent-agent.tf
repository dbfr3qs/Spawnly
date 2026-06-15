resource "spawnly_agent_template" "parent_agent" {
  agent_type      = "parent-agent"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true

  meta {
    display_name = "Parent Agent"
    description  = "Spawns a child agent, retrieves a random string via A2A, then exits"
  }

  runtime_spec {
    image = "agent-parent-agent:latest"

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
    allowed_child_types = ["child-agent"]
    grantable_scopes    = ["sample-api-b:read"]
    max_depth           = 3
  }
}
