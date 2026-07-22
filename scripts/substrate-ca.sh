#!/usr/bin/env bash
# scripts/substrate-ca.sh — ensure the LOCAL-SIM x509pop dev CA exists as a
# cluster Secret. IDEMPOTENT: if the Secret already exists it is reused (so
# previously issued org leaves keep verifying); otherwise a throwaway CA is
# generated and stored.
#
# ⚠ THROWAWAY DEV CA. This is NOT the real Spawnly CA (that is the spawnly-infra
# edge track). The CA cert+key live ONLY in the `substrate-dev-ca` Secret in the
# spire-system namespace — never written to the repo, never baked into an image.
# The generator writes to a mode-0700 temp dir that is removed on exit.
#
# Secret keys:
#   ca.crt  — the CA certificate (the x509pop server trusts this as ca_bundle_path)
#   ca.key  — the CA private key  (used ONLY by scripts/substrate-dev-cert.sh to
#             sign org leaves; NEVER mounted into the SPIRE server)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NS="${SPIRE_NAMESPACE:-spire-system}"
SECRET="${SUBSTRATE_CA_SECRET:-substrate-dev-ca}"
CA_TTL="${SUBSTRATE_CA_TTL:-720h}"

if kubectl -n "$NS" get secret "$SECRET" >/dev/null 2>&1; then
  echo "==> Dev CA Secret ${NS}/${SECRET} already exists — reusing."
  exit 0
fi

echo "==> Generating throwaway x509pop dev CA (valid ${CA_TTL})..."
TMP="$(mktemp -d)"
chmod 700 "$TMP"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

(cd "$REPO_ROOT" && go run ./cmd/substrate-certgen \
  -mode ca -ttl "$CA_TTL" \
  -cert-out "$TMP/ca.crt" -key-out "$TMP/ca.key")

kubectl -n "$NS" create secret generic "$SECRET" \
  --from-file=ca.crt="$TMP/ca.crt" \
  --from-file=ca.key="$TMP/ca.key"

echo "==> Stored dev CA in Secret ${NS}/${SECRET} (ca.crt + ca.key)."
