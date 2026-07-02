#!/usr/bin/env bash
# scripts/keycloak-probe.sh
#
# Keycloak parity probe (kept through Phase 6). Proves the agent-token flow works
# end-to-end against Keycloak with a REAL registered agent, without cutting the
# fleet over (agents still mint from IdentityServer for their real work).
#
# Phase 1 asserted: a real SPIRE JWT-SVID mints a client-credentials token via
# the custom "spawnly-attestation" client authenticator; a garbage assertion is
# invalid_client.
#
# Phase 2 EXTENDS it (this file) to the registry-driven identity:
#   [1] a REAL registered agent's SVID mints a token carrying sub=user:<userId>,
#       a nested act={"sub":"<spiffe>"}, the right scopes and audiences;
#   [2] that token is ACCEPTED end-to-end by sample-api-a (Keycloak issuer via
#       the extended dual-issuer shim) — 200;
#   [3] a token minted with audience=delegation carries token_use=delegation and
#       is REJECTED by sample-api-a (401);
#   [4] a garbage assertion is rejected invalid_client;
#   [5] after the agent is REVOKED, its SVID can no longer mint a token
#       (invalid_client) — the registry liveness gate.
#
# HOW IT GETS A REAL REGISTERED AGENT (the Phase-1 synthetic probe pod would now
# FAIL the liveness gate, since its agentId is not a registry record): it launches
# a probe pod carrying the same labels + CSI workload-API mount a real agent pod
# gets, so SPIRE issues it a spiffe://cluster.local/agent/... id; then it
# SELF-REGISTERS that agent in the registry over POST /v1/agents using a
# registry-audience SVID — the exact registration a real agent-sidecar performs
# on boot (status=active, agentType from the request, SpiceDB tuples written).
# From that point the agent is indistinguishable from an orchestrator-spawned one
# for the purposes of the token endpoint + sample-api authorization.
set -euo pipefail

NS=default
# client_id == agentType. chain-worker holds sample-api-a:read (so its token can
# call sample-api-a) and requiresTenant, exercising the full tenant chain authz.
CLIENT_ID="${CLIENT_ID:-chain-worker}"
AGENT_TYPE="${CLIENT_ID}"
TENANT="${TENANT:-tenant-1}"
USER="${USER_ID:-alice}"
AGENT_ID="${AGENT_ID:-probe-$(date +%s)}"
PROBE_POD="keycloak-probe-${AGENT_ID}"
KC_LOCAL_PORT="${KC_LOCAL_PORT:-18081}"
REG_LOCAL_PORT="${REG_LOCAL_PORT:-18086}"
API_LOCAL_PORT="${API_LOCAL_PORT:-18087}"
JWT_BEARER="urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
SPIRE_IMAGE="ghcr.io/spiffe/spire-agent:1.15.1"

PF_PIDS=()
cleanup() {
  kubectl delete pod "$PROBE_POD" -n "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  for p in "${PF_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

# fetch_svid <audience> — echo a JWT-SVID for the given audience from the pod.
fetch_svid() {
  local aud="$1" raw svid
  for _ in $(seq 1 15); do
    raw=$(kubectl exec -n "$NS" "$PROBE_POD" -- \
      /opt/spire/bin/spire-agent api fetch jwt -audience "$aud" \
      -socketPath /spiffe-workload-api/spire-agent.sock 2>/dev/null || true)
    svid=$(printf '%s\n' "$raw" | grep -oE '[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' | head -n1 || true)
    [ -n "$svid" ] && { printf '%s' "$svid"; return 0; }
    sleep 2
  done
  return 1
}

# decode_payload <jwt> — echo the decoded JSON payload of a JWT. base64url has
# no padding, so re-pad to a multiple of 4 before decoding, and swallow base64's
# non-zero "invalid input" exit on the (harmless) trailing bytes so it runs ONCE
# (a `|| fallback` would decode twice and emit doubled JSON).
decode_payload() {
  local b64
  b64=$(printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+')
  case $(( ${#b64} % 4 )) in
    2) b64="${b64}==" ;;
    3) b64="${b64}=" ;;
  esac
  printf '%s' "$b64" | base64 -d 2>/dev/null || true
}

echo "==> Launching probe pod ${PROBE_POD} (SPIRE-attested as ${AGENT_TYPE}/${AGENT_ID})..."
kubectl apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${PROBE_POD}
  labels:
    agent-platform.io/managed: "true"
    agent-platform.io/scope: "tenant"
    tenant-id: "${TENANT}"
    user-id: "${USER}"
    agent-type: "${AGENT_TYPE}"
    agent-id: "${AGENT_ID}"
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: ${SPIRE_IMAGE}
      command: ["/opt/spire/bin/spire-agent", "api", "watch", "-socketPath", "/spiffe-workload-api/spire-agent.sock"]
      volumeMounts:
        - name: spiffe-workload-api
          mountPath: /spiffe-workload-api
          readOnly: true
  volumes:
    - name: spiffe-workload-api
      csi:
        driver: csi.spiffe.io
        readOnly: true
YAML

echo "==> Waiting for the probe pod to be Ready (SPIRE attestation)..."
kubectl wait --for=condition=Ready "pod/${PROBE_POD}" -n "$NS" --timeout=120s

echo "==> Fetching a registry-audience JWT-SVID (for self-registration)..."
REG_SVID=$(fetch_svid registry) || { echo "FAIL: no registry SVID" >&2; exit 1; }
echo "==> Fetching a keys-audience JWT-SVID (for token minting)..."
SVID=$(fetch_svid keys) || { echo "FAIL: no keys SVID" >&2; exit 1; }
SPIFFE_ID=$(decode_payload "$SVID" | jq -r '.sub' 2>/dev/null || echo "?")
echo "  SVID subject: ${SPIFFE_ID}"

echo "==> Port-forwarding registry, sample-api-a, keycloak..."
kubectl port-forward -n "$NS" svc/registry "${REG_LOCAL_PORT}:8080" >/dev/null 2>&1 & PF_PIDS+=("$!")
kubectl port-forward -n "$NS" svc/sample-api-a "${API_LOCAL_PORT}:8080" >/dev/null 2>&1 & PF_PIDS+=("$!")
kubectl port-forward -n "$NS" svc/keycloak "${KC_LOCAL_PORT}:8080" >/dev/null 2>&1 & PF_PIDS+=("$!")

# wait_ready <url> — poll until the forward accepts connections (or fail loudly).
wait_ready() {
  for _ in $(seq 1 45); do
    curl -sf --max-time 3 -o /dev/null "$1" && return 0
    sleep 1
  done
  echo "FAIL: $1 not reachable through the port-forward" >&2; exit 1
}
wait_ready "http://localhost:${KC_LOCAL_PORT}/realms/spawnly/.well-known/openid-configuration"
wait_ready "http://localhost:${REG_LOCAL_PORT}/v1/templates"

REGISTRY_URL="http://localhost:${REG_LOCAL_PORT}"
SAMPLE_API="http://localhost:${API_LOCAL_PORT}"
TOKEN_URL="http://localhost:${KC_LOCAL_PORT}/realms/spawnly/protocol/openid-connect/token"
# Every curl below carries --max-time so a flapping forward fails the check
# loudly instead of hanging the probe.
CURL="curl -s --max-time 20"

echo ""
echo "==> Self-registering the agent in the registry (real sidecar path)..."
REG_BODY=$(mktemp)
REG_CODE=$($CURL -o "$REG_BODY" -w '%{http_code}' -X POST "${REGISTRY_URL}/v1/agents" \
  -H "Authorization: Bearer ${REG_SVID}" \
  -H "Content-Type: application/json" \
  -d "{\"agentType\":\"${AGENT_TYPE}\",\"tenantId\":\"${TENANT}\",\"userId\":\"${USER}\"}")
if [ "$REG_CODE" != "201" ] && [ "$REG_CODE" != "409" ]; then
  echo "FAIL: registration returned ${REG_CODE}. Body:" >&2; cat "$REG_BODY" >&2; echo >&2; exit 1
fi
echo "  registered (HTTP ${REG_CODE}); registry now has agent ${AGENT_ID} status=active."

echo ""
echo "==> [1/5] Minting a client-credentials token with the REAL SVID..."
BODY=$(mktemp)
CODE=$($CURL -o "$BODY" -w '%{http_code}' -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" \
  -d "client_id=${CLIENT_ID}" \
  -d "scope=sample-api-a:read" \
  -d "client_assertion_type=${JWT_BEARER}" \
  --data-urlencode "client_assertion=${SVID}")
if [ "$CODE" != "200" ]; then
  echo "FAIL: expected 200, got ${CODE}. Body:" >&2; cat "$BODY" >&2; echo >&2; exit 1
fi
ACCESS_TOKEN=$(jq -r '.access_token' <"$BODY")
PAYLOAD=$(decode_payload "$ACCESS_TOKEN")
SUB=$(printf '%s' "$PAYLOAD" | jq -r '.sub')
ACT_SUB=$(printf '%s' "$PAYLOAD" | jq -r '.act.sub')
SCOPES=$(printf '%s' "$PAYLOAD" | jq -r '.scope')
AUDS=$(printf '%s' "$PAYLOAD" | jq -rc '.aud')
echo "  sub=${SUB}  act.sub=${ACT_SUB}"
echo "  scope=\"${SCOPES}\"  aud=${AUDS}"
[ "$SUB" = "user:${USER}" ] || { echo "FAIL: sub != user:${USER}" >&2; exit 1; }
[ "$ACT_SUB" = "$SPIFFE_ID" ] || { echo "FAIL: act.sub != SVID subject" >&2; exit 1; }
printf '%s' "$SCOPES" | grep -q "sample-api-a:read" || { echo "FAIL: missing sample-api-a:read scope" >&2; exit 1; }
printf '%s' "$AUDS" | grep -q "sample-api-a" || { echo "FAIL: aud missing sample-api-a" >&2; exit 1; }
echo "  PASS: sub=user:<id>, nested act.sub, scope + aud all correct."

echo ""
echo "==> [2/5] sample-api-a must ACCEPT the Keycloak agent token (200)..."
API_BODY=$(mktemp)
API_CODE=$($CURL -o "$API_BODY" -w '%{http_code}' "${SAMPLE_API}/work" \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H "X-Tenant-ID: ${TENANT}")
echo "  HTTP ${API_CODE}, body: $(cat "$API_BODY")"
[ "$API_CODE" = "200" ] || { echo "FAIL: sample-api-a rejected the Keycloak agent token" >&2; exit 1; }
echo "  PASS: sample-api-a accepted a Keycloak-minted agent token end-to-end."

echo ""
echo "==> [3/5] audience=delegation ⇒ token_use=delegation, REJECTED by sample-api-a..."
DBODY=$(mktemp)
DCODE=$($CURL -o "$DBODY" -w '%{http_code}' -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" \
  -d "client_id=${CLIENT_ID}" \
  -d "scope=sample-api-a:read" \
  -d "audience=delegation" \
  -d "client_assertion_type=${JWT_BEARER}" \
  --data-urlencode "client_assertion=${SVID}")
[ "$DCODE" = "200" ] || { echo "FAIL: delegation mint returned ${DCODE}" >&2; cat "$DBODY" >&2; exit 1; }
DTOKEN=$(jq -r '.access_token' <"$DBODY")
DUSE=$(decode_payload "$DTOKEN" | jq -r '.token_use')
echo "  token_use=${DUSE}"
[ "$DUSE" = "delegation" ] || { echo "FAIL: token_use != delegation" >&2; exit 1; }
DAPI_CODE=$($CURL -o /dev/null -w '%{http_code}' "${SAMPLE_API}/work" \
  -H "Authorization: Bearer ${DTOKEN}" -H "X-Tenant-ID: ${TENANT}")
echo "  sample-api-a HTTP ${DAPI_CODE}"
[ "$DAPI_CODE" = "401" ] || { echo "FAIL: delegation token not rejected (got ${DAPI_CODE})" >&2; exit 1; }
echo "  PASS: token_use=delegation set, and sample-api-a rejected it (401)."

echo ""
echo "==> [4/5] A garbage assertion must be rejected invalid_client..."
BAD_BODY=$(mktemp)
BAD_CODE=$($CURL -o "$BAD_BODY" -w '%{http_code}' -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" -d "client_id=${CLIENT_ID}" \
  -d "client_assertion_type=${JWT_BEARER}" \
  --data-urlencode "client_assertion=not-a-valid-svid")
BAD_ERR=$(jq -r '.error' <"$BAD_BODY" 2>/dev/null || echo "?")
echo "  HTTP ${BAD_CODE}, error=${BAD_ERR}"
[ "$BAD_CODE" != "200" ] && [ "$BAD_ERR" = "invalid_client" ] \
  || { echo "FAIL: garbage assertion not rejected invalid_client" >&2; cat "$BAD_BODY" >&2; exit 1; }
echo "  PASS: garbage assertion rejected invalid_client."

echo ""
echo "==> [5/5] REVOKE the agent, then its SVID must no longer mint a token..."
$CURL -o /dev/null -X POST "${REGISTRY_URL}/v1/agents/${AGENT_ID}/revoke?userId=${USER}"
# Registry status flips synchronously; the gate reads it live.
REV_BODY=$(mktemp)
REV_CODE=$($CURL -o "$REV_BODY" -w '%{http_code}' -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" -d "client_id=${CLIENT_ID}" \
  -d "scope=sample-api-a:read" \
  -d "client_assertion_type=${JWT_BEARER}" \
  --data-urlencode "client_assertion=${SVID}")
REV_ERR=$(jq -r '.error' <"$REV_BODY" 2>/dev/null || echo "?")
echo "  HTTP ${REV_CODE}, error=${REV_ERR}"
[ "$REV_CODE" != "200" ] && [ "$REV_ERR" = "invalid_client" ] \
  || { echo "FAIL: revoked agent still minted a token (liveness gate)" >&2; cat "$REV_BODY" >&2; exit 1; }
echo "  PASS: revoked agent rejected invalid_client (liveness gate)."

echo ""
echo "==> keycloak-probe: ALL CHECKS PASSED"
