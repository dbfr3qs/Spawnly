resource "spawnly_agent_template" "currency_converter" {
  agent_type      = "currency-converter"
  version         = "1.0.0"
  status          = "active"
  requires_tenant = true

  meta {
    display_name = "Currency Converter"
    description  = "Long-lived A2A server that performs a delegated, read-only conversion"
  }

  runtime_spec {
    image     = "agent-currency-converter:latest"
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
