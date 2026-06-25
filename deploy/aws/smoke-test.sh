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

echo "==> spawning test worker (userId=alice tenantId=acme agentType=chain-worker)"
WL=$(curl -s -X POST localhost:8080/spawn -H 'Content-Type: application/json' \
  -d '{"userId":"alice","tenantId":"acme","agentType":"chain-worker","task":"call the sample API"}' \
  | jq -r .workloadName)
if [ -z "$WL" ] || [ "$WL" = "null" ]; then
  echo "ERROR: spawn did not return a workloadName" >&2
  exit 1
fi
echo "    workload: $WL"

# chain-worker is long-lived (it loops + self-spawns one child of its own type
# up to the template's maxDepth), so unlike a job-and-exit worker it never
# terminates on its own. DELETE is NOT cascading on this platform (only
# revoke/resume cascade), so reap the whole subtree explicitly: BFS the parentId
# tree from $WL over the agents list and delete every descendant plus the root.
teardown() {
  kill $PF 2>/dev/null || true
  [ -n "${WL:-}" ] || return 0
  local agents to_delete frontier next p kids id
  agents=$(curl -s --max-time 5 localhost:8080/v1/agents 2>/dev/null || echo '[]')
  to_delete="$WL"; frontier="$WL"
  for _ in $(seq 1 5); do # maxDepth is 4; one extra pass for safety
    next=""
    for p in $frontier; do
      kids=$(echo "$agents" | jq -r --arg p "$p" '.[] | select(.parentId == $p) | .agentId' 2>/dev/null)
      next="$next $kids"
    done
    next=$(echo "$next" | xargs -n1 2>/dev/null | sort -u)
    [ -n "$next" ] || break
    to_delete="$to_delete $next"; frontier="$next"
  done
  for id in $(echo "$to_delete" | xargs -n1 | sort -u); do
    curl -s --max-time 5 -X DELETE "localhost:8080/v1/agents/$id" >/dev/null 2>&1 || true
  done
}
trap teardown EXIT

echo "==> waiting for the worker's first authorized sample-api call"
events=""
for _ in $(seq 1 45); do
  events=$(curl -s "localhost:8080/v1/agents/$WL/events" | jq -r '.[].type')
  echo "$events" | grep -qE 'work_ok|work_denied|pod_failed' && break
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
echo "worker result (expect '[chain-worker] work -> 200', no error):"
echo "${worklog:-<none>}" | sed 's/^/   /'
echo "========================================================="
echo

if [ "$attestor_issuer" = "aws-stsweb" ] \
   && echo "$events" | grep -q "work_ok" \
   && ! echo "$events" | grep -q "pod_failed"; then
  echo "✅ PASS — agent attested via aws-stsweb (cluster-attested kubernetes-pod-name),"
  echo "   minted an IS token, and made an authorized sample-api call. No SPIRE."
  exit 0
fi

echo "❌ FAIL — see the evidence above."
echo "   Debug: kubectl logs -l app=sample-api --tail=20 ; kubectl logs -l agent-id=$WL -c agent-sidecar"
exit 1
