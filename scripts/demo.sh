#!/usr/bin/env bash
# scripts/demo.sh
set -euo pipefail

DASHBOARD_PORT=8090
ORCH_PORT=8080

echo "==> Cleaning up any stale port-forwards..."
pkill -f "kubectl port-forward" 2>/dev/null || true
sleep 1

echo "==> Port-forwarding orchestrator (localhost:${ORCH_PORT}) and dashboard (localhost:${DASHBOARD_PORT})..."
kubectl port-forward svc/orchestrator ${ORCH_PORT}:8080 &
PF_ORCH=$!
kubectl port-forward svc/dashboard ${DASHBOARD_PORT}:8080 &
PF_DASH=$!
sleep 2

echo ""
echo "  Dashboard: http://localhost:${DASHBOARD_PORT}"
echo ""

echo "==> Spawning demo agent (chain-worker) with task 'hello from the demo'..."
RESPONSE=$(curl -sf -X POST http://localhost:${ORCH_PORT}/spawn \
  -H 'Content-Type: application/json' \
  -d '{"userId":"user-1","tenantId":"tenant-1","agentType":"chain-worker","task":"hello from the demo"}')

echo "Spawn response: $RESPONSE"
AGENT_ID=$(echo "$RESPONSE" | jq -r '.workloadName')
echo "Agent ID: $AGENT_ID"

echo ""
# chain-worker is long-lived (it loops + self-spawns one child of its own type),
# so it never "Completes" — wait until it makes its first authorized sample-api
# call (a work_ok event), which proves the SVID -> token -> protected-call path.
echo "==> Waiting for the agent's first authorized API call (watching in background)..."
kubectl get agentworkloads -w &
WATCH_PID=$!

for i in $(seq 1 30); do
  EVENTS=$(curl -sf "http://localhost:${ORCH_PORT}/v1/agents/${AGENT_ID}/events" 2>/dev/null | jq -r '.[].type' 2>/dev/null || echo "")
  echo "  [$i] events: $(echo "$EVENTS" | tr '\n' ' ')"
  if echo "$EVENTS" | grep -qE 'work_ok|work_denied'; then
    break
  fi
  PHASE=$(kubectl get agentworkload "$AGENT_ID" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
  [ "$PHASE" = "Failed" ] && break
  sleep 3
done

kill $WATCH_PID 2>/dev/null || true

echo ""
echo "==> Agent pod logs:"
kubectl logs "${AGENT_ID}-pod" 2>/dev/null || echo "(pod already cleaned up)"

echo ""
echo "==> Lifecycle events via API:"
curl -sf http://localhost:${ORCH_PORT}/v1/agents/${AGENT_ID}/events \
  | jq '[.[] | {source: .source, type: .type, time: .timestamp}]' 2>/dev/null \
  || echo "(no events found)"

echo ""
echo "================================================================"
echo "  Dashboard: http://localhost:${DASHBOARD_PORT}"
echo "  Click on '${AGENT_ID}' to see the full event timeline,"
echo "  including decoded JWT tokens, SpiceDB relations, and API calls."
echo "================================================================"
echo ""
echo "Port-forwards are still running. Press Ctrl+C or run:"
echo "  pkill -f 'kubectl port-forward'"

# Keep port-forwards alive until user exits
wait $PF_ORCH $PF_DASH
