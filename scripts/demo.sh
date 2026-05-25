#!/usr/bin/env bash
# scripts/demo.sh
set -euo pipefail

echo "==> Port-forwarding orchestrator..."
kubectl port-forward svc/orchestrator 8080:8080 &
PF_ORCH=$!
sleep 2

echo "==> Spawning agent..."
RESPONSE=$(curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"userId":"user-1","tenantId":"tenant-1","agentType":"worker"}')

echo "Spawn response: $RESPONSE"
AGENT_ID=$(echo "$RESPONSE" | jq -r '.workloadName')
echo "Workload name: $AGENT_ID"

echo ""
echo "==> Watching AgentWorkload lifecycle (Ctrl+C to stop)..."
kubectl get agentworkloads -w &
WATCH_PID=$!

echo ""
echo "==> Waiting for agent pod to complete..."
for i in $(seq 1 30); do
  PHASE=$(kubectl get agentworkload "$AGENT_ID" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  echo "  phase: $PHASE"
  if [ "$PHASE" = "Completed" ] || [ "$PHASE" = "Failed" ]; then
    break
  fi
  sleep 3
done

kill $WATCH_PID 2>/dev/null || true

echo ""
echo "==> Agent pod logs:"
kubectl logs "$AGENT_ID-pod" 2>/dev/null || echo "(pod already cleaned up)"

echo ""
echo "==> Checking registry (port-forwarding)..."
kubectl port-forward svc/registry 8081:8080 &
PF_REG=$!
sleep 2

curl -sf http://localhost:8081/v1/agents/$AGENT_ID | jq . 2>/dev/null || echo "(agent record not found)"

kill $PF_ORCH $PF_REG 2>/dev/null || true

echo ""
echo "Demo complete."
