#!/usr/bin/env bash
# scripts/keycloak-probe.sh
#
# Phase 1 probe (kept through Phase 6): prove that a REAL SPIRE JWT-SVID mints a
# client-credentials token from Keycloak via the custom "spawnly-attestation"
# client authenticator, and that a garbage assertion is rejected invalid_client.
#
# It does NOT depend on the fleet being on Keycloak (the fleet stays on
# IdentityServer in Phase 1) — it exercises Keycloak directly with a genuine
# SVID minted by SPIRE.
#
# How it gets a real SVID without a running agent: it launches a short-lived
# probe pod carrying the same labels + CSI workload-API mount an agent pod gets,
# so SPIRE's ClusterSPIFFEID issues it a spiffe://cluster.local/agent/... id.
# The probe pod runs the SPIRE agent image (which ships the `spire-agent` CLI),
# so we can `kubectl exec` it to fetch a JWT-SVID for the "keys" audience.
#
# Later phases (2+) extend this script with sub=user:<id>/act assertions,
# delegation, and CIBA. Phase 1 asserts only: 200 + decodable JWT for a real
# SVID, and invalid_client for a bad one.
set -euo pipefail

NS=default
# client_id must be one of the six machine clients switched to the
# spawnly-attestation authenticator in deploy/keycloak/clients.tf.
CLIENT_ID="${CLIENT_ID:-weather-monitor}"
# The SVID subject SPIFFE id — shaped like a real weather-monitor agent so the
# agentId derivation (last path segment) is exercised. Phase 1 does no registry
# lookup, so this need not correspond to a live registry record yet.
TENANT="${TENANT:-tenant-1}"
USER="${USER_ID:-alice}"
AGENT_TYPE="weather-monitor"
AGENT_ID="${AGENT_ID:-probe-$(date +%s)}"
PROBE_POD="keycloak-probe-${AGENT_ID}"
KC_LOCAL_PORT="${KC_LOCAL_PORT:-18081}"
JWT_BEARER="urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
SPIRE_IMAGE="ghcr.io/spiffe/spire-agent:1.15.1"

cleanup() {
  kubectl delete pod "$PROBE_POD" -n "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  [ -n "${PF_PID:-}" ] && kill "$PF_PID" 2>/dev/null || true
}
trap cleanup EXIT

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
      # Keep the container alive so we can exec the CLI against the socket.
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

echo "==> Fetching a JWT-SVID (audience=keys) from the probe pod..."
# `spire-agent api fetch jwt` prints the token on its own line after a
# "token(...)" header line. Grab the first long base64url.JWT-looking line.
# SPIRE can take a few seconds AFTER the pod is Ready to deliver the SVID over
# the workload API, so retry a handful of times.
SVID=""
for _ in $(seq 1 15); do
  SVID_RAW=$(kubectl exec -n "$NS" "$PROBE_POD" -- \
    /opt/spire/bin/spire-agent api fetch jwt -audience keys \
    -socketPath /spiffe-workload-api/spire-agent.sock 2>/dev/null || true)
  SVID=$(printf '%s\n' "$SVID_RAW" | grep -oE '[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+' | head -n1 || true)
  [ -n "$SVID" ] && break
  sleep 2
done
if [ -z "$SVID" ]; then
  echo "FAIL: could not obtain a JWT-SVID from the probe pod. Raw output:" >&2
  printf '%s\n' "$SVID_RAW" >&2
  exit 1
fi
SPIFFE_ID=$(printf '%s' "$SVID" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null | jq -r '.sub' 2>/dev/null || echo "?")
echo "  got SVID for subject: ${SPIFFE_ID}"

echo "==> Port-forwarding svc/keycloak to localhost:${KC_LOCAL_PORT}..."
kubectl port-forward -n "$NS" svc/keycloak "${KC_LOCAL_PORT}:8080" >/dev/null 2>&1 &
PF_PID=$!
# Wait for the forward to accept connections.
for _ in $(seq 1 30); do
  curl -sf -o /dev/null "http://localhost:${KC_LOCAL_PORT}/realms/spawnly/.well-known/openid-configuration" && break
  sleep 1
done

TOKEN_URL="http://localhost:${KC_LOCAL_PORT}/realms/spawnly/protocol/openid-connect/token"

echo ""
echo "==> [1/2] Minting a client-credentials token with the REAL SVID..."
HTTP_BODY=$(mktemp)
CODE=$(curl -s -o "$HTTP_BODY" -w '%{http_code}' -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" \
  -d "client_id=${CLIENT_ID}" \
  -d "client_assertion_type=${JWT_BEARER}" \
  --data-urlencode "client_assertion=${SVID}")
echo "  HTTP ${CODE}"
if [ "$CODE" != "200" ]; then
  echo "FAIL: expected 200, got ${CODE}. Body:" >&2
  cat "$HTTP_BODY" >&2; echo >&2
  exit 1
fi
ACCESS_TOKEN=$(jq -r '.access_token' <"$HTTP_BODY")
if [ -z "$ACCESS_TOKEN" ] || [ "$ACCESS_TOKEN" = "null" ]; then
  echo "FAIL: 200 but no access_token in the response body." >&2
  cat "$HTTP_BODY" >&2; echo >&2
  exit 1
fi
# Decode the JWT header+payload to prove it is a real, decodable token.
HDR=$(printf '%s' "$ACCESS_TOKEN" | cut -d. -f1 | tr '_-' '/+' | base64 -d 2>/dev/null | jq -c . 2>/dev/null || echo "")
PAYLOAD=$(printf '%s' "$ACCESS_TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null || \
          printf '%s' "$ACCESS_TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d --ignore-garbage 2>/dev/null || echo "")
if [ -z "$HDR" ]; then
  echo "FAIL: minted token is not a decodable JWT." >&2
  exit 1
fi
echo "  PASS: minted a decodable JWT. header=${HDR}"
echo "        iss=$(printf '%s' "$PAYLOAD" | jq -r '.iss' 2>/dev/null) azp=$(printf '%s' "$PAYLOAD" | jq -r '.azp' 2>/dev/null)"

echo ""
echo "==> [2/2] A garbage assertion must be rejected invalid_client..."
BAD_BODY=$(mktemp)
BAD_CODE=$(curl -s -o "$BAD_BODY" -w '%{http_code}' -X POST "$TOKEN_URL" \
  -d "grant_type=client_credentials" \
  -d "client_id=${CLIENT_ID}" \
  -d "client_assertion_type=${JWT_BEARER}" \
  --data-urlencode "client_assertion=not-a-valid-svid")
BAD_ERR=$(jq -r '.error' <"$BAD_BODY" 2>/dev/null || echo "?")
echo "  HTTP ${BAD_CODE}, error=${BAD_ERR}"
if [ "$BAD_CODE" = "200" ] || [ "$BAD_ERR" != "invalid_client" ]; then
  echo "FAIL: garbage assertion was NOT rejected as invalid_client." >&2
  cat "$BAD_BODY" >&2; echo >&2
  exit 1
fi
echo "  PASS: garbage assertion rejected invalid_client."

echo ""
echo "==> keycloak-probe: ALL CHECKS PASSED"
