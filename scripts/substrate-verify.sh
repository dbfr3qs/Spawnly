#!/usr/bin/env bash
# scripts/substrate-verify.sh — prove the Phase-2 identity loop on a live cluster
# (remote-substrate). Run AFTER scripts/substrate-setup.sh. Asserts:
#
#   (a) the x509pop agent attested and its NODE landed under the org path
#       spiffe://cluster.local/spire/agent/x509pop/org/<org>/<fingerprint>
#   (b) a sim POD gets the org-scoped workload SVID
#       spiffe://cluster.local/org/<org>/agent/... and that SVID VERIFIES through
#       the Phase-1 Go verifier (cmd/substrate-verify) against a matching org
#   (c) [negative] a leaf signed by a DIFFERENT/forged CA FAILS node attestation
#       (the agent never gets a node SVID)
#
# This is the scripted integration wired into the Phase-6 harness / e2e-setup.
# ⚠ LOCAL-SIM / e2e only.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORG="${1:-${SUBSTRATE_ORG:-acme}}"
SUBSTRATE_NS="${SUBSTRATE_NAMESPACE:-substrate-sim}"
SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"
TD="${SPIRE_TRUST_DOMAIN:-cluster.local}"
SERVER_POD="spire-server-0"
SERVER="kubectl -n ${SPIRE_NS} exec ${SERVER_POD} -c spire-server -- /opt/spire/bin/spire-server"

IDENTITY_SECRET="${SUBSTRATE_LEAF_SECRET:-substrate-identity}"
fail() { echo "FAIL: $*" >&2; exit 1; }

# ── (a) node attested under the org path ──────────────────────────────────────
# Derive the EXACT node id deterministically from the CURRENT identity leaf rather
# than picking head -1 of the org-prefixed nodes — otherwise a stale node from a
# prior run (different fingerprint) could be selected, parenting the proof entry to
# a dead agent. SPIRE's x509pop fingerprint is sha1(DER-of-leaf) hex, which is what
# the agent_path_template embeds.
FP="$(kubectl -n "$SUBSTRATE_NS" get secret "$IDENTITY_SECRET" -o jsonpath='{.data.svid\.crt}' 2>/dev/null \
  | base64 -d | openssl x509 -outform der 2>/dev/null | sha1sum | awk '{print $1}')"
[ -n "$FP" ] || fail "could not fingerprint the substrate identity leaf (Secret ${SUBSTRATE_NS}/${IDENTITY_SECRET})"
NODE_ID="spiffe://${TD}/spire/agent/x509pop/org/${ORG}/${FP}"

echo "==> (a) waiting for the x509pop node ${NODE_ID} to attest..."
found=0
for _ in $(seq 1 30); do
  if $SERVER agent list 2>/dev/null | grep -qF "$NODE_ID"; then found=1; break; fi
  sleep 2
done
[ "$found" = "1" ] || fail "expected node ${NODE_ID} did not attest — check the substrate agent logs"
echo "    node attested: ${NODE_ID}"

# ── create a script-created registration entry for a proof pod ────────────────
# (The CP-mediated Entry API replaces this in Phase 3.) SPIFFE ID conforms to the
# canonical org-scoped contract (internal/spiffe.BuildOrgAgentPath):
#   spiffe://<td>/org/<org>/agent/<tenant>/<user>/<type>/<agentId>
POD="substrate-proof"
AGENT_PATH="spiffe://${TD}/org/${ORG}/agent/${SUBSTRATE_NS}/tester/proof/${POD}"
echo "==> creating script registration entry ${AGENT_PATH} (parent ${NODE_ID})..."
# Idempotent, but ONLY tolerate the already-exists case — a real failure (RBAC,
# malformed id, server down) must surface here, not 40 lines later as a confusing
# SVID mismatch.
if ! ENTRY_OUT="$($SERVER entry create \
  -parentID "$NODE_ID" \
  -spiffeID "$AGENT_PATH" \
  -selector "k8s:ns:${SUBSTRATE_NS}" \
  -selector "k8s:pod-name:${POD}" 2>&1)"; then
  printf '%s\n' "$ENTRY_OUT" | grep -qi "already exists" || fail "entry create failed: ${ENTRY_OUT}"
fi

# ── (b) proof pod gets the org-scoped SVID, verified by the Phase-1 verifier ──
echo "==> (b) launching proof pod and fetching its org-scoped SVID..."
kubectl -n "$SUBSTRATE_NS" delete pod "$POD" --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true
cat <<YAML | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: ${SUBSTRATE_NS}
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: ghcr.io/spiffe/spire-agent:1.15.1
      command: ["/opt/spire/bin/spire-agent", "api", "watch", "-socketPath", "/spiffe-workload-api/spire-agent.sock"]
      volumeMounts:
        - name: spiffe-workload-api
          mountPath: /spiffe-workload-api
          readOnly: true
  volumes:
    - name: spiffe-workload-api
      csi:
        driver: csi.substrate.spiffe.io
        readOnly: true
YAML
kubectl -n "$SUBSTRATE_NS" wait --for=condition=Ready "pod/${POD}" --timeout=90s

# X509-SVID identity: assert the SPIFFE ID equals the org-scoped path.
GOT_ID="$(kubectl -n "$SUBSTRATE_NS" exec "$POD" -c probe -- \
  /opt/spire/bin/spire-agent api fetch x509 -socketPath /spiffe-workload-api/spire-agent.sock 2>/dev/null \
  | grep -oE "spiffe://${TD}/org/${ORG}/agent/\S+" | head -1 || true)"
[ "$GOT_ID" = "$AGENT_PATH" ] || fail "pod SVID id ${GOT_ID:-<none>} != expected ${AGENT_PATH}"
echo "    pod SVID id: ${GOT_ID}"

# JWT-SVID → Phase-1 Go verifier (real registry path). Port-forward the OIDC JWKS.
echo "==> verifying the SVID through the Phase-1 Go verifier (cmd/substrate-verify)..."
TOKEN="$(kubectl -n "$SUBSTRATE_NS" exec "$POD" -c probe -- \
  /opt/spire/bin/spire-agent api fetch jwt -audience registry \
  -socketPath /spiffe-workload-api/spire-agent.sock 2>/dev/null \
  | sed -n '2p' | tr -d '[:space:]')"
[ -n "$TOKEN" ] || fail "could not fetch a JWT-SVID from the proof pod"

JWKS_PORT="${SUBSTRATE_JWKS_PORT:-18443}"
kubectl -n "$SPIRE_NS" port-forward svc/spire-spiffe-oidc-discovery-provider "${JWKS_PORT}:443" >/dev/null 2>&1 &
PF_PID=$!
trap 'kill "$PF_PID" 2>/dev/null || true; kubectl -n "$SUBSTRATE_NS" delete pod "$POD" --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true' EXIT
# Poll the JWKS endpoint until the forward is actually serving, rather than a blind
# sleep that races a slow/relocated forward.
for _ in $(seq 1 20); do
  curl -sk "https://localhost:${JWKS_PORT}/keys" >/dev/null 2>&1 && break
  sleep 1
done

(cd "$REPO_ROOT" && go run ./cmd/substrate-verify \
  -token "$TOKEN" \
  -jwks-url "https://localhost:${JWKS_PORT}/keys" \
  -org "$ORG" -trust-domain "$TD") || fail "Phase-1 verifier did not accept the org-scoped SVID"
echo "    Phase-1 verifier accepted the SVID and bound org=${ORG}."

# ── (c) forged-CA leaf fails node attestation ─────────────────────────────────
echo "==> (c) negative: a forged-CA leaf must FAIL node attestation..."
"$REPO_ROOT/scripts/substrate-verify-forged.sh" "$ORG" \
  && echo "    forged-CA leaf was correctly REJECTED (no node)." \
  || fail "forged-CA negative check failed"

echo ""
echo "PASS: Phase-2 substrate identity loop verified for org '${ORG}'."
