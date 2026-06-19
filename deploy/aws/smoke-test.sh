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

ready=""
for _ in $(seq 1 30); do
  curl -sf --max-time 3 -o /dev/null localhost:8080/v1/agents && { ready=1; break; }
  sleep 1
done
if [ -z "$ready" ]; then
  echo "ERROR: orchestrator not reachable on :8080 (port-forward failed — stale forward holding the port?)" >&2
  echo "       pkill -f 'kubectl port-forward' ; check: kubectl get pods -l app=orchestrator" >&2
  exit 1
fi

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
# Hardened attestor stamps issuer=aws-stsweb and the agent id is derived from the
# cluster-attested kubernetes-pod-name (not a self-asserted session name).
attestor_issuer=$(printf '%s' "$reg" | sed -n 's/.*issuer=\([^ ]*\).*/\1/p')
worklog=$(kubectl logs -l "agent-id=$WL" -c agent --tail=10 2>/dev/null)

echo ""
echo "================ STS ATTESTATION EVIDENCE ================"
echo "registry registration (expect issuer=aws-stsweb, agent id from attested pod name):"
echo "   ${reg:-<not found>}"
echo
echo "agent event timeline:"
echo "${events:-<none>}" | sed 's/^/   /'
echo
echo "worker result (expect 'task result: ...', no error):"
echo "${worklog:-<none>}" | sed 's/^/   /'
echo "========================================================="
echo

if [ "$attestor_issuer" = "aws-stsweb" ] \
   && echo "$worklog" | grep -q "task result:" \
   && ! echo "$events" | grep -q "pod_failed"; then
  echo "✅ PASS — agent attested via aws-stsweb (cluster-attested kubernetes-pod-name),"
  echo "   minted an IS token, and made an authorized sample-api call. No SPIRE."
  exit 0
fi

echo "❌ FAIL — see the evidence above."
echo "   Debug: kubectl logs -l app=sample-api --tail=20 ; kubectl logs -l agent-id=$WL -c agent-sidecar"
exit 1
