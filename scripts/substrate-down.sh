#!/usr/bin/env bash
# scripts/substrate-down.sh — tear down the remote-substrate local sim
# (Phase 2 overlay + Phase 6 two-org harness). Best-effort + idempotent.
#
# Removes the deploy/substrate overlay, the per-run proof/forged pods, and org B's
# credential Secret. By DEFAULT it LEAVES the dev CA Secret, the server's x509pop
# attestor, and the shared Entry-API socket in place (re-run substrate-e2e-setup to
# bring the sim straight back). Pass SUBSTRATE_UNWIRE=1 to also unwire the Entry API
# delegated-admin socket and return spire-server + orchestrator to their seed.
#
# ⚠ LOCAL-SIM / e2e only.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SUBSTRATE_NS="${SUBSTRATE_NAMESPACE:-substrate-sim}"
SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"
ORCH_NS="${ORCH_NAMESPACE:-default}"

echo "==> Removing the substrate overlay..."
kubectl delete -k "$REPO_ROOT/deploy/substrate" --ignore-not-found >/dev/null 2>&1 || true

echo "==> Sweeping per-run proof/forged pods + org-B credential Secret..."
if kubectl get ns "$SUBSTRATE_NS" >/dev/null 2>&1; then
  kubectl -n "$SUBSTRATE_NS" delete pod substrate-proof substrate-isolation-proof substrate-forged-agent \
    --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true
  # org-B identity Secrets (substrate-identity-<org>) — the isolation-matrix creds.
  for s in $(kubectl -n "$SUBSTRATE_NS" get secret -o name 2>/dev/null | grep -E 'substrate-identity-' || true); do
    kubectl -n "$SUBSTRATE_NS" delete "$s" --ignore-not-found >/dev/null 2>&1 || true
  done
  kubectl -n "$SUBSTRATE_NS" delete secret substrate-forged-identity --ignore-not-found >/dev/null 2>&1 || true
fi

if [ "${SUBSTRATE_UNWIRE:-0}" = "1" ]; then
  echo "==> Unwiring the Entry API delegated-admin socket (SUBSTRATE_UNWIRE=1)..."
  # Drop SPIRE_SERVER_SOCKET + the shared-socket mount from the orchestrator.
  kubectl -n "$ORCH_NS" set env deployment/orchestrator SPIRE_SERVER_SOCKET- >/dev/null 2>&1 || true
  if kubectl -n "$ORCH_NS" get deployment orchestrator -o jsonpath='{.spec.template.spec.volumes[?(@.name=="spire-server-socket")].name}' 2>/dev/null | grep -q spire-server-socket; then
    idx="$(kubectl -n "$ORCH_NS" get deployment orchestrator -o json | jq '.spec.template.spec.volumes | map(.name=="spire-server-socket") | index(true)')"
    midx="$(kubectl -n "$ORCH_NS" get deployment orchestrator -o json | jq '.spec.template.spec.containers[0].volumeMounts | map(.name=="spire-server-socket") | index(true)')"
    [ "$midx" != "null" ] && kubectl -n "$ORCH_NS" patch deployment orchestrator --type=json \
      -p "[{\"op\":\"remove\",\"path\":\"/spec/template/spec/containers/0/volumeMounts/${midx}\"}]" >/dev/null 2>&1 || true
    [ "$idx" != "null" ] && kubectl -n "$ORCH_NS" patch deployment orchestrator --type=json \
      -p "[{\"op\":\"remove\",\"path\":\"/spec/template/spec/volumes/${idx}\"}]" >/dev/null 2>&1 || true
  fi
  kubectl -n "$ORCH_NS" rollout status deployment/orchestrator --timeout=120s >/dev/null 2>&1 || true
  # Return the spire-server socket volume to an emptyDir (its chart default).
  sidx="$(kubectl -n "$SPIRE_NS" get statefulset spire-server -o json 2>/dev/null | jq '.spec.template.spec.volumes | map(.name=="spire-server-socket") | index(true)')"
  if [ -n "$sidx" ] && [ "$sidx" != "null" ]; then
    kubectl -n "$SPIRE_NS" patch statefulset spire-server --type=json \
      -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/volumes/${sidx}\",\"value\":{\"name\":\"spire-server-socket\",\"emptyDir\":{}}}]" >/dev/null 2>&1 || true
    kubectl -n "$SPIRE_NS" rollout status statefulset/spire-server --timeout=180s >/dev/null 2>&1 || true
  fi
fi

echo "Done. Substrate sim torn down."
