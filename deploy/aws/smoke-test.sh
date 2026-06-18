#!/usr/bin/env bash
# Spawn the test worker and print evidence that AWS-STS attestation works:
# the registry registration (issuer=aws-sts + assumed-role ARN), the agent's
# event timeline, and the worker's result from the authorized sample-api call.
# Exits 0 only if all three confirm success. Reusable on its own once the
# platform is deployed.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)"

echo "==> port-forwarding orchestrator (:8080)"
kubectl port-forward svc/orchestrator 8080:8080 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT

for _ in $(seq 1 30); do
  curl -sf -o /dev/null localhost:8080/v1/agents && break
  sleep 1
done

echo "==> spawning test worker (userId=alice tenantId=acme agentType=worker)"
WL=$(curl -s -X POST localhost:8080/spawn -H 'Content-Type: application/json' \
  -d '{"userId":"alice","tenantId":"acme","agentType":"worker","task":"call the sample API"}' \
  | jq -r .workloadName)
if [ -z "$WL" ] || [ "$WL" = "null" ]; then
  echo "ERROR: spawn did not return a workloadName" >&2
  exit 1
fi
echo "    workload: $WL"

echo "==> waiting for the worker to finish"
events=""
for _ in $(seq 1 45); do
  events=$(curl -s "localhost:8080/v1/agents/$WL/events" | jq -r '.[].type')
  echo "$events" | grep -qE 'stopping|pod_failed' && break
  sleep 2
done

reg=$(kubectl logs -l app=registry --tail=400 2>/dev/null | grep "registering agent $WL" | tail -1)
worklog=$(kubectl logs -l "agent-id=$WL" -c agent --tail=10 2>/dev/null)

echo ""
echo "================ STS ATTESTATION EVIDENCE ================"
echo "registry registration (expect issuer=aws-sts + assumed-role ARN):"
echo "   ${reg:-<not found>}"
echo
echo "agent event timeline:"
echo "${events:-<none>}" | sed 's/^/   /'
echo
echo "worker result (expect 'task result: ...', no error):"
echo "${worklog:-<none>}" | sed 's/^/   /'
echo "========================================================="
echo

if echo "$reg" | grep -q "issuer=aws-sts" \
   && echo "$worklog" | grep -q "task result:" \
   && ! echo "$events" | grep -q "pod_failed"; then
  echo "✅ PASS — agent attested via AWS STS, minted an IS token, and made an"
  echo "   authorized sample-api call. No SPIFFE/SPIRE in the cluster."
  exit 0
fi

echo "❌ FAIL — see the evidence above."
echo "   Debug: kubectl logs -l app=sample-api --tail=20 ; kubectl logs -l agent-id=$WL -c agent-sidecar"
exit 1
