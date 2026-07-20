#!/usr/bin/env bash
# scripts/connect-substrate.sh — connect an org's substrate via the Phase-3
# CP-mediated connect API (remote-substrate). This REPLACES the Phase-2 stand-in
# scripts/substrate-dev-cert.sh: instead of signing a leaf locally off the dev CA,
# it asks the control plane — authenticated with your delegated org-admin token —
# to ISSUE the org-bound x509 identity. Admin auth + CP issuance IS the org
# binding (no proof-of-control step), exactly like scripts/activate-anchor.sh's
# ergonomics for anchors.
#
# The one-time identity (leaf cert + key + bootstrap trust bundle) is returned
# ONCE in the response and stored straight into the substrate namespace Secret the
# SPIRE agent mounts (svid.crt / svid.key / bundle.crt). Spawnly retains only the
# cert fingerprint. Nothing is written to the repo or an image.
#
# Usage:
#   ORCHESTRATOR_URL=https://spawnly.run \
#   ACCESS_TOKEN=<org-admin bearer token> \
#   ./scripts/connect-substrate.sh <orgId> [substrate-namespace]
set -euo pipefail

ORG="${1:-}"
: "${ORG:?usage: connect-substrate.sh <orgId> [substrate-namespace] (with ORCHESTRATOR_URL + ACCESS_TOKEN set)}"
: "${ORCHESTRATOR_URL:?set ORCHESTRATOR_URL to the orchestrator/edge base URL}"
: "${ACCESS_TOKEN:?set ACCESS_TOKEN to an org-admin delegated access token}"
SUBSTRATE_NS="${2:-${SUBSTRATE_NAMESPACE:-substrate-sim}}"
LEAF_SECRET="${SUBSTRATE_LEAF_SECRET:-substrate-identity}"

need() { command -v "$1" >/dev/null || { echo "ERROR: $1 is required" >&2; exit 1; }; }
need jq
need kubectl

TMP="$(mktemp -d)"; chmod 700 "$TMP"
trap 'rm -rf "$TMP"' EXIT

echo "==> Requesting an org-bound substrate identity for '${ORG}' via ${ORCHESTRATOR_URL}..."
# -f surfaces a non-2xx as an error exit (403/404 for a non-admin / cross-org).
RESP="$(curl -sf -X POST "${ORCHESTRATOR_URL}/v1/orgs/${ORG}/substrate" \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H 'Content-Type: application/json')"

# -e makes jq exit non-zero when a field is null/absent, so a malformed response
# fails loudly here rather than writing the literal string "null" into the Secret.
echo "$RESP" | jq -er '.identity.certPem'        > "$TMP/svid.crt"
echo "$RESP" | jq -er '.identity.keyPem'         > "$TMP/svid.key"
echo "$RESP" | jq -er '.identity.trustBundlePem' > "$TMP/bundle.crt"
NODE_ID="$(echo "$RESP" | jq -er '.nodeId')"
FP="$(echo "$RESP" | jq -er '.certFingerprint')"
grep -q 'BEGIN CERTIFICATE' "$TMP/svid.crt" && grep -q 'PRIVATE KEY' "$TMP/svid.key" \
  || { echo "ERROR: connect response missing identity material" >&2; exit 1; }

echo "==> Storing the one-time identity in Secret ${SUBSTRATE_NS}/${LEAF_SECRET}..."
kubectl create namespace "$SUBSTRATE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$SUBSTRATE_NS" create secret generic "$LEAF_SECRET" \
  --from-file=svid.crt="$TMP/svid.crt" \
  --from-file=svid.key="$TMP/svid.key" \
  --from-file=bundle.crt="$TMP/bundle.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

echo ""
echo "Substrate '${ORG}' connected (pending)."
echo "  cert fingerprint : ${FP}"
echo "  x509pop node id  : ${NODE_ID}"
echo "  Install the deploy/substrate bundle; its SPIRE agent will attest as the node above."
