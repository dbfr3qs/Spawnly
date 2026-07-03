#!/usr/bin/env bash
# scripts/keycloak-probe.sh
#
# Keycloak parity probe (kept as a doctor/debug tool). Proves the agent-token
# flow works end-to-end against Keycloak with a REAL registered agent:
# client-credentials mint via the attestation SPI, registry-driven identity,
# RFC 8693 delegation, and CIBA spawn-consent. The full e2e/ suite now covers
# these flows against the live fleet; this script stays for standalone
# troubleshooting of the IdP path.
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
echo "==> [Phase 3] RFC 8693 token-exchange delegation probe..."
# Proves the custom SpawnlyTokenExchangeProvider end-to-end against Keycloak:
#   a PARENT (travel-planner) mints an audience=delegation token; a CHILD
#   (flight-search) exchanges it with its OWN SVID as actor_token, and Keycloak
#   returns a token whose `act` WRAPS the child around the parent. Then each
#   rejection is exercised live: scope not in the subject token (invalid_scope),
#   a scope over the registry ceiling (invalid_grant), a disallowed edge
#   (invalid_grant), and a revoked chain member (invalid_grant).
#
# Delegation policy needs a grantable edge whose scope the PARENT client can
# actually mint into the subject_token. chain-worker self-delegates
# (allowedChildTypes:[chain-worker]) and holds sample-api-a:read, so it is the
# natural edge: parent and child are both chain-worker clients (distinct agent
# ids). The example chain-worker template ships grantableScopes:[] (nothing
# carryable), so we TEMPORARILY upsert it with grantableScopes:["sample-api-a:read"]
# (maxDepth 2) and RESTORE the original in cleanup. This mutates only the
# in-memory registry template, not any running agent.

CP_TOKEN=$(kubectl get secret control-plane-auth -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
CP_HDR=()
[ -n "$CP_TOKEN" ] && CP_HDR=(-H "Authorization: Bearer ${CP_TOKEN}")

PARENT_TYPE="chain-worker"
CHILD_TYPE="chain-worker"
DELEG_SCOPE="sample-api-a:read"
# A scope the parent CAN mint (chain-worker holds orchestrator:spawn) but that is
# OVER the delegation ceiling — used for the invalid_grant ceiling check.
OVER_SCOPE="orchestrator:spawn"
# A scope the parent CANNOT put in the subject at all is not needed; the
# scope-subset check is exercised by requesting a scope absent from the subject.
TP_ORIG=$(mktemp)
TP_RESTORE_NEEDED=

restore_template() {
  if [ -n "$TP_RESTORE_NEEDED" ] && [ -s "$TP_ORIG" ]; then
    curl -s --max-time 20 -X POST "${REGISTRY_URL}/v1/templates" \
      -H 'Content-Type: application/json' "${CP_HDR[@]}" \
      --data-binary @"$TP_ORIG" >/dev/null 2>&1 || true
  fi
}
# Chain template restore onto the existing cleanup trap.
trap 'restore_template; cleanup' EXIT

echo "==> Saving the ${PARENT_TYPE} template and upserting a grantable edge..."
curl -s --max-time 20 "${CP_HDR[@]}" "${REGISTRY_URL}/v1/templates/${PARENT_TYPE}" -o "$TP_ORIG"
if ! jq -e .agentType "$TP_ORIG" >/dev/null 2>&1; then
  echo "FAIL: could not read ${PARENT_TYPE} template" >&2; cat "$TP_ORIG" >&2; exit 1
fi
TP_PATCHED=$(mktemp)
jq --arg s "$DELEG_SCOPE" '.delegation.grantableScopes = [$s] | .delegation.maxDepth = 2' \
  "$TP_ORIG" >"$TP_PATCHED"
UP_CODE=$(curl -s --max-time 20 -o /dev/null -w '%{http_code}' -X POST "${REGISTRY_URL}/v1/templates" \
  -H 'Content-Type: application/json' "${CP_HDR[@]}" --data-binary @"$TP_PATCHED")
[ "$UP_CODE" = "201" ] || { echo "FAIL: template upsert returned ${UP_CODE}" >&2; exit 1; }
TP_RESTORE_NEEDED=1
echo "  ${PARENT_TYPE} now delegates ${DELEG_SCOPE} to ${CHILD_TYPE} (maxDepth 2)."

# launch_probe_pod <pod-name> <agent-type> <agent-id> — SPIRE-attested probe pod.
launch_probe_pod() {
  local pod="$1" atype="$2" aid="$3"
  kubectl apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    agent-platform.io/managed: "true"
    agent-platform.io/scope: "tenant"
    tenant-id: "${TENANT}"
    user-id: "${USER}"
    agent-type: "${atype}"
    agent-id: "${aid}"
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
  kubectl wait --for=condition=Ready "pod/${pod}" -n "$NS" --timeout=120s >/dev/null
}

# fetch_svid_from <pod> <audience> — like fetch_svid but for an arbitrary pod.
fetch_svid_from() {
  local pod="$1" aud="$2" raw svid
  for _ in $(seq 1 15); do
    raw=$(kubectl exec -n "$NS" "$pod" -- \
      /opt/spire/bin/spire-agent api fetch jwt -audience "$aud" \
      -socketPath /spiffe-workload-api/spire-agent.sock 2>/dev/null || true)
    svid=$(printf '%s\n' "$raw" | grep -oE '[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' | head -n1 || true)
    [ -n "$svid" ] && { printf '%s' "$svid"; return 0; }
    sleep 2
  done
  return 1
}

# register_agent <agent-type> <svid> — self-register; echoes nothing, exits on fail.
register_agent() {
  local atype="$1" svid="$2" body code
  body=$(mktemp)
  code=$($CURL -o "$body" -w '%{http_code}' -X POST "${REGISTRY_URL}/v1/agents" \
    -H "Authorization: Bearer ${svid}" -H "Content-Type: application/json" \
    -d "{\"agentType\":\"${atype}\",\"tenantId\":\"${TENANT}\",\"userId\":\"${USER}\"}")
  if [ "$code" != "201" ] && [ "$code" != "409" ]; then
    echo "FAIL: register ${atype} returned ${code}" >&2; cat "$body" >&2; exit 1
  fi
}

PARENT_ID="pd-parent-$(date +%s)"
CHILD_ID="pd-child-$(date +%s)"
PARENT_POD="keycloak-probe-${PARENT_ID}"
CHILD_POD="keycloak-probe-${CHILD_ID}"
# Extend cleanup to also delete these two pods.
cleanup_deleg() {
  kubectl delete pod "$PARENT_POD" "$CHILD_POD" -n "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap 'cleanup_deleg; restore_template; cleanup' EXIT

echo "==> Launching parent (${PARENT_TYPE}) + child (${CHILD_TYPE}) probe pods..."
launch_probe_pod "$PARENT_POD" "$PARENT_TYPE" "$PARENT_ID"
launch_probe_pod "$CHILD_POD" "$CHILD_TYPE" "$CHILD_ID"

PARENT_REG_SVID=$(fetch_svid_from "$PARENT_POD" registry) || { echo "FAIL: no parent registry SVID" >&2; exit 1; }
CHILD_REG_SVID=$(fetch_svid_from "$CHILD_POD" registry) || { echo "FAIL: no child registry SVID" >&2; exit 1; }
register_agent "$PARENT_TYPE" "$PARENT_REG_SVID"
register_agent "$CHILD_TYPE" "$CHILD_REG_SVID"
echo "  registered parent=${PARENT_ID} (${PARENT_TYPE}) child=${CHILD_ID} (${CHILD_TYPE})."

PARENT_SVID=$(fetch_svid_from "$PARENT_POD" keys) || { echo "FAIL: no parent keys SVID" >&2; exit 1; }
CHILD_SVID=$(fetch_svid_from "$CHILD_POD" keys) || { echo "FAIL: no child keys SVID" >&2; exit 1; }
CHILD_SPIFFE=$(decode_payload "$CHILD_SVID" | jq -r '.sub')
PARENT_SPIFFE=$(decode_payload "$PARENT_SVID" | jq -r '.sub')
echo "  parent spiffe=${PARENT_SPIFFE}"
echo "  child  spiffe=${CHILD_SPIFFE}"

EX_GRANT="urn:ietf:params:oauth:grant-type:token-exchange"
ACCESS_TT="urn:ietf:params:oauth:token-type:access_token"
JWT_TT="urn:ietf:params:oauth:token-type:jwt"

# mint_delegation_token <client_id> <svid> <scope> — echoes an audience=delegation
# token (the subject_token a parent hands down), exits on failure.
mint_delegation_token() {
  local cid="$1" svid="$2" scope="$3" body code
  body=$(mktemp)
  code=$($CURL -o "$body" -w '%{http_code}' -X POST "$TOKEN_URL" \
    -d "grant_type=client_credentials" -d "client_id=${cid}" \
    -d "scope=${scope}" -d "audience=delegation" \
    -d "client_assertion_type=${JWT_BEARER}" --data-urlencode "client_assertion=${svid}")
  [ "$code" = "200" ] || { echo "FAIL: parent delegation mint ${code}" >&2; cat "$body" >&2; exit 1; }
  jq -r '.access_token' <"$body"
}

# exchange <subject_token> <scope> <audience> — child runs the exchange with its
# own SVID as actor_token; echoes "HTTPCODE<TAB>BODYFILE".
exchange() {
  local subject="$1" scope="$2" aud="$3" body code
  body=$(mktemp)
  local args=(-o "$body" -w '%{http_code}' -X POST "$TOKEN_URL"
    -d "grant_type=${EX_GRANT}"
    --data-urlencode "subject_token=${subject}" -d "subject_token_type=${ACCESS_TT}"
    --data-urlencode "actor_token=${CHILD_SVID}" -d "actor_token_type=${JWT_TT}"
    -d "client_id=${CHILD_TYPE}"
    -d "client_assertion_type=${JWT_BEARER}" --data-urlencode "client_assertion=${CHILD_SVID}"
    -d "scope=${scope}")
  [ -n "$aud" ] && args+=(-d "audience=${aud}")
  code=$($CURL "${args[@]}")
  # Trailing newline so `read` returns 0 (no-newline EOF makes read exit 1, which
  # would abort the caller under `set -e`).
  printf '%s\t%s\n' "$code" "$body"
}

echo ""
echo "==> [6/9] Parent mints a delegation token; child exchanges it (wrapped act)..."
SUBJECT=$(mint_delegation_token "$PARENT_TYPE" "$PARENT_SVID" "$DELEG_SCOPE")
# The subject_token's act.sub is the PARENT spiffe (Phase 2 mapper). That is the
# delegation parent the exchange resolves.
SUBJ_ACT=$(decode_payload "$SUBJECT" | jq -r '.act.sub')
echo "  subject_token act.sub=${SUBJ_ACT}"
[ "$SUBJ_ACT" = "$PARENT_SPIFFE" ] || { echo "FAIL: subject act.sub != parent spiffe" >&2; exit 1; }

IFS=$'\t' read -r EX_CODE EX_BODY < <(exchange "$SUBJECT" "$DELEG_SCOPE" "")
if [ "$EX_CODE" != "200" ]; then
  echo "FAIL: exchange returned ${EX_CODE}" >&2; cat "$EX_BODY" >&2; echo >&2; exit 1
fi
EX_TOKEN=$(jq -r '.access_token' <"$EX_BODY")
EX_PAYLOAD=$(decode_payload "$EX_TOKEN")
EX_SUB=$(printf '%s' "$EX_PAYLOAD" | jq -r '.sub')
EX_ACT_SUB=$(printf '%s' "$EX_PAYLOAD" | jq -r '.act.sub')
EX_ACT_INNER=$(printf '%s' "$EX_PAYLOAD" | jq -r '.act.act.sub')
EX_SCOPE=$(printf '%s' "$EX_PAYLOAD" | jq -r '.scope')
EX_AUD=$(printf '%s' "$EX_PAYLOAD" | jq -rc '.aud')
echo "  sub=${EX_SUB}"
echo "  act.sub=${EX_ACT_SUB}  act.act.sub=${EX_ACT_INNER}"
echo "  scope=\"${EX_SCOPE}\"  aud=${EX_AUD}"
[ "$EX_SUB" = "user:${USER}" ] || { echo "FAIL: exchanged sub != user:${USER}" >&2; exit 1; }
[ "$EX_ACT_SUB" = "$CHILD_SPIFFE" ] || { echo "FAIL: outer act.sub != child (not wrapped)" >&2; exit 1; }
[ "$EX_ACT_INNER" = "$PARENT_SPIFFE" ] || { echo "FAIL: inner act.act.sub != parent (chain lost)" >&2; exit 1; }
printf '%s' "$EX_SCOPE" | grep -q "${DELEG_SCOPE}" || { echo "FAIL: exchanged scope missing ${DELEG_SCOPE}" >&2; exit 1; }
printf '%s' "$EX_AUD" | grep -q "sample-api-a" || { echo "FAIL: exchanged aud missing sample-api-a" >&2; exit 1; }
echo "  PASS: act wraps child around parent; scope + aud correct; sub carried over."

echo ""
echo "==> [7/9] Requesting a scope NOT in the subject_token ⇒ invalid_scope..."
# subject only carries ${DELEG_SCOPE}; also request a scope it does not carry.
IFS=$'\t' read -r S_CODE S_BODY < <(exchange "$SUBJECT" "${DELEG_SCOPE} ${OVER_SCOPE}" "")
S_ERR=$(jq -r '.error' <"$S_BODY" 2>/dev/null || echo "?")
echo "  HTTP ${S_CODE}, error=${S_ERR}"
[ "$S_CODE" != "200" ] && [ "$S_ERR" = "invalid_scope" ] \
  || { echo "FAIL: scope-not-in-subject not rejected invalid_scope" >&2; cat "$S_BODY" >&2; exit 1; }
echo "  PASS: scope-subset enforced (invalid_scope)."

echo ""
echo "==> [8/9] Requesting a scope OVER the delegation ceiling ⇒ invalid_grant..."
# Mint a subject carrying ${OVER_SCOPE} (in the parent's optional scopes), then
# request it: it's in the subject but NOT in grantableScopes:[${DELEG_SCOPE}].
SUBJECT2=$(mint_delegation_token "$PARENT_TYPE" "$PARENT_SVID" "$OVER_SCOPE")
IFS=$'\t' read -r C_CODE C_BODY < <(exchange "$SUBJECT2" "$OVER_SCOPE" "")
C_ERR=$(jq -r '.error' <"$C_BODY" 2>/dev/null || echo "?")
echo "  HTTP ${C_CODE}, error=${C_ERR}"
[ "$C_CODE" != "200" ] && [ "$C_ERR" = "invalid_grant" ] \
  || { echo "FAIL: over-ceiling scope not rejected invalid_grant" >&2; cat "$C_BODY" >&2; exit 1; }
echo "  PASS: delegation ceiling enforced (invalid_grant)."

echo ""
echo "==> [9/9] Revoke an ANCESTOR (the parent), then the exchange ⇒ invalid_grant..."
# Revoke the PARENT, not the child: the child still authenticates (client-auth
# passes the liveness gate), so the exchange runs and its OWN chain-revocation
# walk finds the parent (an ancestor in the act chain) revoked ⇒ invalid_grant.
# This is the frozen "revocation covers ALL ancestors, not just the immediate
# actor" semantic. (Revoking the CHILD instead is also blocked, but earlier — at
# client-auth, as invalid_client — which the Phase-2 [5/5] check already covers.)
$CURL -o /dev/null -X POST "${REGISTRY_URL}/v1/agents/${PARENT_ID}/revoke?userId=${USER}"
IFS=$'\t' read -r R_CODE R_BODY < <(exchange "$SUBJECT" "$DELEG_SCOPE" "")
R_ERR=$(jq -r '.error' <"$R_BODY" 2>/dev/null || echo "?")
echo "  HTTP ${R_CODE}, error=${R_ERR}"
[ "$R_CODE" != "200" ] && [ "$R_ERR" = "invalid_grant" ] \
  || { echo "FAIL: revoked ancestor did not block the exchange" >&2; cat "$R_BODY" >&2; exit 1; }
echo "  PASS: chain-revocation covers ancestors (invalid_grant)."

echo ""
echo "==> [Phase 4] CIBA spawn-consent probe..."
# Proves the custom SpawnlyAuthenticationChannelProvider + completion poller
# end-to-end against Keycloak's CIBA backchannel + token-poll:
#   [10] a consent-gated child opens a backchannel request → PENDING (no stored
#        consent); the token poll returns authorization_pending;
#   [11] a dashboard/registry APPROVE releases the token (sub=user:<id>, act.sub
#        = the child SVID) — real user-bound token, minted by the CIBA poll;
#   [12] a SECOND run for the SAME edge AUTO-APPROVES from the now-stored consent
#        (no human step) — resolves on the first poll;
#   [13] a fresh edge that is DENIED fails the poll with access_denied;
#   [14] a REVOKED agent cannot redeem an approved auth_req_id (client-auth on the
#        CIBA poll rejects the revoked agent — revocation starves it).
#
# The edge is parentType->childType = travel-planner -> chain-worker. chain-worker
# is CIBA-enabled (oidc.ciba.grant.enabled) and holds sample-api-a:read; the child
# is registered WITH a parentId (a real travel-planner agent) so the channel
# provider resolves parentType from the registry — the edge is bound from the
# VERIFIED SVID + registry, never from the request's login_hint/params.

CIBA_URL="http://localhost:${KC_LOCAL_PORT}/realms/spawnly/protocol/openid-connect/ext/ciba/auth"
CIBA_GRANT="urn:openid:params:grant-type:ciba"
CONSENT_SCOPE="openid sample-api-a:read"
CIBA_PARENT_TYPE="travel-planner"
CIBA_CHILD_TYPE="chain-worker"

# Fresh agents so the edge starts with NO stored consent.
CB_PARENT_ID="cb-parent-$(date +%s)"
CB_CHILD_ID="cb-child-$(date +%s)"
CB_PARENT_POD="keycloak-probe-${CB_PARENT_ID}"
CB_CHILD_POD="keycloak-probe-${CB_CHILD_ID}"
cleanup_ciba() {
  kubectl delete pod "$CB_PARENT_POD" "$CB_CHILD_POD" -n "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap 'cleanup_ciba; cleanup_deleg; restore_template; cleanup' EXIT

echo "==> Launching CIBA parent (${CIBA_PARENT_TYPE}) + child (${CIBA_CHILD_TYPE}) pods..."
launch_probe_pod "$CB_PARENT_POD" "$CIBA_PARENT_TYPE" "$CB_PARENT_ID"
launch_probe_pod "$CB_CHILD_POD" "$CIBA_CHILD_TYPE" "$CB_CHILD_ID"

CB_PARENT_REG_SVID=$(fetch_svid_from "$CB_PARENT_POD" registry) || { echo "FAIL: no CIBA parent registry SVID" >&2; exit 1; }
CB_CHILD_REG_SVID=$(fetch_svid_from "$CB_CHILD_POD" registry) || { echo "FAIL: no CIBA child registry SVID" >&2; exit 1; }

# register_agent_with_parent <type> <svid> <parentId> — self-register with a parent
# so the registry records the child's parentId (the spawn edge).
register_agent_with_parent() {
  local atype="$1" svid="$2" parent="$3" body code
  body=$(mktemp)
  code=$($CURL -o "$body" -w '%{http_code}' -X POST "${REGISTRY_URL}/v1/agents" \
    -H "Authorization: Bearer ${svid}" -H "Content-Type: application/json" \
    -d "{\"agentType\":\"${atype}\",\"tenantId\":\"${TENANT}\",\"userId\":\"${USER}\",\"parentId\":\"${parent}\"}")
  if [ "$code" != "201" ] && [ "$code" != "409" ]; then
    echo "FAIL: register ${atype} (parent=${parent}) returned ${code}" >&2; cat "$body" >&2; exit 1
  fi
}

# The parent registers as a root travel-planner; the child registers under it.
register_agent "$CIBA_PARENT_TYPE" "$CB_PARENT_REG_SVID"
register_agent_with_parent "$CIBA_CHILD_TYPE" "$CB_CHILD_REG_SVID" "$CB_PARENT_ID"
echo "  registered parent=${CB_PARENT_ID} (${CIBA_PARENT_TYPE}) child=${CB_CHILD_ID} (${CIBA_CHILD_TYPE}) under parent."

CB_CHILD_SVID=$(fetch_svid_from "$CB_CHILD_POD" keys) || { echo "FAIL: no CIBA child keys SVID" >&2; exit 1; }
CB_CHILD_SPIFFE=$(decode_payload "$CB_CHILD_SVID" | jq -r '.sub')
echo "  child spiffe=${CB_CHILD_SPIFFE}"

# ciba_initiate — open a backchannel request; echoes the auth_req_id (exits on fail).
ciba_initiate() {
  local body code
  body=$(mktemp)
  code=$($CURL -o "$body" -w '%{http_code}' -X POST "$CIBA_URL" \
    -d "client_id=${CIBA_CHILD_TYPE}" \
    -d "client_assertion_type=${JWT_BEARER}" --data-urlencode "client_assertion=${CB_CHILD_SVID}" \
    --data-urlencode "scope=${CONSENT_SCOPE}" \
    -d "login_hint=user:${USER}" \
    -d "binding_message=spawnly-consent")
  if [ "$code" != "200" ]; then
    echo "FAIL: CIBA backchannel request returned ${code}" >&2; cat "$body" >&2; exit 1
  fi
  jq -r '.auth_req_id' <"$body"
}

# ciba_poll <auth_req_id> — poll the token endpoint once; echoes "HTTPCODE<TAB>BODYFILE".
ciba_poll() {
  local arid="$1" body code
  body=$(mktemp)
  code=$($CURL -o "$body" -w '%{http_code}' -X POST "$TOKEN_URL" \
    -d "grant_type=${CIBA_GRANT}" -d "auth_req_id=${arid}" \
    -d "client_id=${CIBA_CHILD_TYPE}" \
    -d "client_assertion_type=${JWT_BEARER}" --data-urlencode "client_assertion=${CB_CHILD_SVID}")
  printf '%s\t%s\n' "$code" "$body"
}

# registry_consent_request_id_for <externalRef> — the auth_req_id Keycloak returns
# is a JWE; the registry request is correlated by externalRef = the CIBA
# authResultId (a DIFFERENT id, internal to Keycloak). So we instead find the
# PENDING request for this user+edge via the registry list API.
pending_request_id() {
  local body
  body=$(mktemp)
  $CURL -o "$body" "${REGISTRY_URL}/v1/consent-requests?userId=${USER}&status=pending" "${CP_HDR[@]}"
  jq -r --arg p "$CIBA_PARENT_TYPE" --arg c "$CIBA_CHILD_TYPE" \
    'map(select(.parentType==$p and .childType==$c)) | last | .id // empty' <"$body"
}

echo ""
echo "==> [10/14] Backchannel request goes PENDING (no stored consent)..."
ARID=$(ciba_initiate)
[ -n "$ARID" ] || { echo "FAIL: empty auth_req_id" >&2; exit 1; }
echo "  auth_req_id acquired."
IFS=$'\t' read -r P_CODE P_BODY < <(ciba_poll "$ARID")
P_ERR=$(jq -r '.error' <"$P_BODY" 2>/dev/null || echo "?")
echo "  first poll: HTTP ${P_CODE}, error=${P_ERR}"
[ "$P_ERR" = "authorization_pending" ] \
  || { echo "FAIL: expected authorization_pending, got ${P_ERR}" >&2; cat "$P_BODY" >&2; exit 1; }
echo "  PASS: consent-gated request is pending."

echo ""
echo "==> [11/14] Dashboard APPROVE releases the token..."
CR_ID=""
for _ in $(seq 1 10); do CR_ID=$(pending_request_id); [ -n "$CR_ID" ] && break; sleep 1; done
[ -n "$CR_ID" ] || { echo "FAIL: no pending registry consent request found" >&2; exit 1; }
echo "  registry consent request: ${CR_ID}"
AP_CODE=$($CURL -o /dev/null -w '%{http_code}' -X POST \
  "${REGISTRY_URL}/v1/consent-requests/${CR_ID}/approve?userId=${USER}" "${CP_HDR[@]}")
[ "$AP_CODE" = "200" ] || { echo "FAIL: approve returned ${AP_CODE}" >&2; exit 1; }
# Poll until the completion poller flips the device code (poller runs every ~2s).
CB_TOKEN=""
for _ in $(seq 1 15); do
  IFS=$'\t' read -r T_CODE T_BODY < <(ciba_poll "$ARID")
  if [ "$T_CODE" = "200" ]; then CB_TOKEN=$(jq -r '.access_token' <"$T_BODY"); break; fi
  sleep 2
done
[ -n "$CB_TOKEN" ] || { echo "FAIL: token not released after approval" >&2; exit 1; }
CB_PAYLOAD=$(decode_payload "$CB_TOKEN")
CB_SUB=$(printf '%s' "$CB_PAYLOAD" | jq -r '.sub')
CB_ACT_SUB=$(printf '%s' "$CB_PAYLOAD" | jq -r '.act.sub')
echo "  sub=${CB_SUB}  act.sub=${CB_ACT_SUB}"
[ "$CB_SUB" = "user:${USER}" ] || { echo "FAIL: CIBA token sub != user:${USER}" >&2; exit 1; }
[ "$CB_ACT_SUB" = "$CB_CHILD_SPIFFE" ] || { echo "FAIL: CIBA token act.sub != child SVID" >&2; exit 1; }
echo "  PASS: approval released a user-bound token with the child in act."

echo ""
echo "==> [12/14] SECOND run for the same edge AUTO-APPROVES from stored consent..."
ARID2=$(ciba_initiate)
CB_TOKEN2=""
for _ in $(seq 1 15); do
  IFS=$'\t' read -r T2_CODE T2_BODY < <(ciba_poll "$ARID2")
  if [ "$T2_CODE" = "200" ]; then CB_TOKEN2=$(jq -r '.access_token' <"$T2_BODY"); break; fi
  # Must NOT require a human approval this time.
  sleep 2
done
[ -n "$CB_TOKEN2" ] || { echo "FAIL: stored consent did not auto-approve the second run" >&2; exit 1; }
echo "  PASS: stored consent auto-approved the second run (no human step)."

echo ""
echo "==> [13/14] A DENIED request fails the poll with access_denied..."
# Revoke the stored consent so the next request goes pending again, then deny it.
# (Find the consent record id and revoke it.)
CONSENTS_BODY=$(mktemp)
$CURL -o "$CONSENTS_BODY" "${REGISTRY_URL}/v1/consents?userId=${USER}" "${CP_HDR[@]}"
CONSENT_ID=$(jq -r --arg p "$CIBA_PARENT_TYPE" --arg c "$CIBA_CHILD_TYPE" \
  'map(select(.parentType==$p and .childType==$c)) | last | .id // empty' <"$CONSENTS_BODY" 2>/dev/null || true)
if [ -n "$CONSENT_ID" ]; then
  $CURL -o /dev/null -X POST "${REGISTRY_URL}/v1/consents/${CONSENT_ID}/revoke" "${CP_HDR[@]}" || true
fi
ARID3=$(ciba_initiate)
# First poll should be pending again (stored consent revoked).
IFS=$'\t' read -r D0_CODE D0_BODY < <(ciba_poll "$ARID3")
D0_ERR=$(jq -r '.error' <"$D0_BODY" 2>/dev/null || echo "?")
[ "$D0_ERR" = "authorization_pending" ] \
  || { echo "FAIL: expected pending after consent revoke, got ${D0_ERR}" >&2; cat "$D0_BODY" >&2; exit 1; }
CR_ID3=""
for _ in $(seq 1 10); do CR_ID3=$(pending_request_id); [ -n "$CR_ID3" ] && break; sleep 1; done
[ -n "$CR_ID3" ] || { echo "FAIL: no pending request to deny" >&2; exit 1; }
$CURL -o /dev/null -X POST "${REGISTRY_URL}/v1/consent-requests/${CR_ID3}/deny?userId=${USER}" "${CP_HDR[@]}"
DENIED=""
for _ in $(seq 1 15); do
  IFS=$'\t' read -r DN_CODE DN_BODY < <(ciba_poll "$ARID3")
  DN_ERR=$(jq -r '.error' <"$DN_BODY" 2>/dev/null || echo "?")
  if [ "$DN_ERR" = "access_denied" ]; then DENIED=1; break; fi
  sleep 2
done
[ -n "$DENIED" ] || { echo "FAIL: denial did not surface access_denied" >&2; exit 1; }
echo "  PASS: denial failed the poll with access_denied."

echo ""
echo "==> [14/14] A REVOKED agent cannot redeem an approved auth_req_id..."
# Open a fresh request + approve it, but DON'T redeem yet; then revoke the child.
# The child can no longer authenticate on the CIBA token poll (grant-aware
# liveness rejects revoked), so it can never redeem the approved auth_req_id.
ARID4=$(ciba_initiate)
CR_ID4=""
for _ in $(seq 1 10); do CR_ID4=$(pending_request_id); [ -n "$CR_ID4" ] && break; sleep 1; done
[ -n "$CR_ID4" ] || { echo "FAIL: no pending request to approve" >&2; exit 1; }
$CURL -o /dev/null -X POST "${REGISTRY_URL}/v1/consent-requests/${CR_ID4}/approve?userId=${USER}" "${CP_HDR[@]}"
# Now revoke the child agent BEFORE it redeems.
$CURL -o /dev/null -X POST "${REGISTRY_URL}/v1/agents/${CB_CHILD_ID}/revoke?userId=${USER}"
STARVED=""
for _ in $(seq 1 8); do
  IFS=$'\t' read -r RV_CODE RV_BODY < <(ciba_poll "$ARID4")
  RV_ERR=$(jq -r '.error' <"$RV_BODY" 2>/dev/null || echo "?")
  # A revoked agent's client-auth fails ⇒ invalid_client (never a 200 token).
  if [ "$RV_CODE" != "200" ] && [ "$RV_ERR" = "invalid_client" ]; then STARVED=1; break; fi
  # If the poller approved the device code first, a redeem would be a 200 — that
  # would be a FAILURE. Keep trying briefly; a revoked agent must never get 200.
  [ "$RV_CODE" = "200" ] && { echo "FAIL: revoked agent redeemed an approved auth_req_id!" >&2; exit 1; }
  sleep 2
done
[ -n "$STARVED" ] || { echo "FAIL: revoked agent poll did not surface invalid_client" >&2; cat "$RV_BODY" >&2; exit 1; }
echo "  PASS: revoked agent cannot redeem an approved auth_req_id (starved)."

echo ""
echo "==> keycloak-probe: ALL CHECKS PASSED"
