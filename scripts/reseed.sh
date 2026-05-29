#!/usr/bin/env bash
# scripts/reseed.sh — re-seed agent templates into a running registry.
# Run after redeploying the registry (which resets its in-memory store).
set -euo pipefail

echo "==> Port-forwarding registry..."
kubectl port-forward svc/registry 18080:8080 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 2

echo "==> Seeding templates..."

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "worker",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Generic Worker", "description": "Calls the sample API and exits"},
    "runtimeSpec": {
      "image": "agent-agent:latest",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {"LOG_LEVEL": "info", "MAX_RETRIES": "3"}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  worker"

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "weather-monitor",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Weather Monitor", "description": "Long-lived agent that monitors weather on a loop"},
    "runtimeSpec": {
      "image": "agent-weather-monitor:latest",
      "lifecycle": "long-lived",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {"LOG_LEVEL": "info"}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  weather-monitor"

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "parent-agent",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Parent Agent", "description": "Spawns a child agent, retrieves a random string via A2A, then exits"},
    "runtimeSpec": {
      "image": "agent-parent-agent:latest",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    },
    "delegation": {"allowedChildTypes": ["child-agent"], "grantableScopes": ["sample-api-b:read"], "maxDepth": 3}
  }'
echo "  parent-agent"

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "child-agent",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Child Agent", "description": "Long-lived A2A server that generates a random string on request"},
    "runtimeSpec": {
      "image": "agent-child-agent:latest",
      "lifecycle": "long-lived",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  child-agent"

echo ""
echo "Done. All templates seeded."
