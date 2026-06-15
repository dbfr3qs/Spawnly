resource "spawnly_agent_template" "trip_planner" {
  agent_type      = "trip-planner"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true

  meta {
    display_name = "Trip Planner"
    description  = "Spawns a currency converter, delegates a read-only conversion, then exits"
  }

  runtime_spec {
    image = "agent-trip-planner:latest"

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
    allowed_child_types = ["currency-converter"]
    grantable_scopes    = ["sample-api-b:read"]
    max_depth           = 3
  }
}
