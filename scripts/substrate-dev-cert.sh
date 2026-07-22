#!/usr/bin/env bash
# scripts/substrate-dev-cert.sh — issue an org-bound x509pop leaf off the dev CA
# for a given orgId, and store it as a Secret the substrate SPIRE agent presents.
#
# This is the Phase-2 stand-in for Phase-3's CP-mediated issuance (the connect
# API): here the org binding comes from the CP (this script) signing CN=<orgId>
# with the dev CA the server trusts. The substrate agent presenting this leaf
# attests as a node under spiffe://<td>/spire/agent/x509pop/org/<orgId>/...
#
# Usage:
#   ./scripts/substrate-dev-cert.sh <orgId> [substrate-namespace]
#
# ⚠ LOCAL-SIM only; leaf key material lives only in the resulting Secret, never
# in the repo or an image. The generator writes to a mode-0700 temp dir removed on
# exit.
set -euo pipefail

ORG="${1:-}"
: "${ORG:?usage: substrate-dev-cert.sh <orgId> [substrate-namespace]}"
SUBSTRATE_NS="${2:-${SUBSTRATE_NAMESPACE:-substrate-sim}}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"
CA_SECRET="${SUBSTRATE_CA_SECRET:-substrate-dev-ca}"
TRUST_DOMAIN="${SPIRE_TRUST_DOMAIN:-cluster.local}"
LEAF_TTL="${SUBSTRATE_LEAF_TTL:-168h}"
# One identity Secret per org so several orgs can be simulated side by side.
LEAF_SECRET="${SUBSTRATE_LEAF_SECRET:-substrate-identity-${ORG}}"

TMP="$(mktemp -d)"; chmod 700 "$TMP"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

echo "==> Reading dev CA from Secret ${SPIRE_NS}/${CA_SECRET}..."
# Existence check FIRST: under `set -euo pipefail` a missing Secret would abort the
# read pipeline before the friendly hint below could ever print.
if ! kubectl -n "$SPIRE_NS" get secret "$CA_SECRET" >/dev/null 2>&1; then
  echo "ERROR: dev CA Secret ${SPIRE_NS}/${CA_SECRET} missing — run scripts/substrate-ca.sh first" >&2
  exit 1
fi
kubectl -n "$SPIRE_NS" get secret "$CA_SECRET" -o jsonpath='{.data.ca\.crt}' | base64 -d > "$TMP/ca.crt"
kubectl -n "$SPIRE_NS" get secret "$CA_SECRET" -o jsonpath='{.data.ca\.key}' | base64 -d > "$TMP/ca.key"
[ -s "$TMP/ca.crt" ] && [ -s "$TMP/ca.key" ] || { echo "ERROR: dev CA Secret missing — run scripts/substrate-ca.sh" >&2; exit 1; }

echo "==> Signing org-bound leaf for org '${ORG}' (td=${TRUST_DOMAIN}, valid ${LEAF_TTL})..."
(cd "$REPO_ROOT" && go run ./cmd/substrate-certgen \
  -mode leaf -org "$ORG" -trust-domain "$TRUST_DOMAIN" -ttl "$LEAF_TTL" \
  -ca-cert "$TMP/ca.crt" -ca-key "$TMP/ca.key" \
  -cert-out "$TMP/leaf.crt" -key-out "$TMP/leaf.key")

# The substrate SPIRE agent (deploy/substrate) mounts this Secret: svid.crt +
# svid.key are the leaf it presents; bundle.crt is the CA the agent uses to
# authenticate the SERVER's x509pop challenge response.
kubectl create namespace "$SUBSTRATE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n "$SUBSTRATE_NS" create secret generic "$LEAF_SECRET" \
  --from-file=svid.crt="$TMP/leaf.crt" \
  --from-file=svid.key="$TMP/leaf.key" \
  --from-file=bundle.crt="$TMP/ca.crt" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> Stored org '${ORG}' substrate identity in Secret ${SUBSTRATE_NS}/${LEAF_SECRET}."
echo "    The substrate agent will attest under spiffe://${TRUST_DOMAIN}/spire/agent/x509pop/org/${ORG}/<fingerprint>"
