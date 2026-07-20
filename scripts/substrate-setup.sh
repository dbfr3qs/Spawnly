#!/usr/bin/env bash
# scripts/substrate-setup.sh — stand up the LOCAL-SIM substrate bundle
# (remote-substrate Phase 2) on the current kind cluster. IDEMPOTENT.
#
# Steps:
#   1. Enable x509pop on the hosted SPIRE server (+ ensure the dev CA Secret).
#   2. Issue an org-bound leaf for ORG into Secret substrate-sim/substrate-identity.
#   3. Copy the server trust bundle into substrate-sim so the agent can bootstrap.
#   4. Apply the deploy/substrate/ overlay and wait for the agent + CSI + operator.
#
# Usage:  ./scripts/substrate-setup.sh [orgId]     (default: acme, a demo org)
#
# ⚠ LOCAL-SIM / e2e only. Assumes a bootstrapped cluster (make bootstrap) so the
# agent-operator image is already loaded into kind and the demo orgs exist.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORG="${1:-${SUBSTRATE_ORG:-acme}}"
SUBSTRATE_NS="${SUBSTRATE_NAMESPACE:-substrate-sim}"
SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"

echo "==> [1/4] Enabling x509pop on the hosted SPIRE server..."
"$REPO_ROOT/scripts/substrate-spire-x509pop.sh"

echo "==> [2/4] Issuing org '${ORG}' substrate identity into ${SUBSTRATE_NS}/substrate-identity..."
# The overlay's spire-agent mounts a fixed Secret name (substrate-identity), so
# pin it here regardless of org (one substrate == one org in the sim).
SUBSTRATE_LEAF_SECRET="substrate-identity" \
  "$REPO_ROOT/scripts/substrate-dev-cert.sh" "$ORG" "$SUBSTRATE_NS"

echo "==> [3/4] Copying the SPIRE trust bundle into ${SUBSTRATE_NS}..."
kubectl create namespace "$SUBSTRATE_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
# Re-materialize the bundle ConfigMap in substrate-sim from the server-published
# one in spire-system (strip metadata so it applies cleanly into the new ns).
kubectl -n "$SPIRE_NS" get configmap spire-bundle -o json \
  | jq '{apiVersion,kind,metadata:{name:"spire-bundle",namespace:"'"$SUBSTRATE_NS"'"},data}' \
  | kubectl apply -f -

echo "==> [4/4] Applying the substrate overlay..."
# The substrate operator + agent call the control-plane-gated registry (template
# reads, preregister lookups). Copy the platform's control-plane-auth Secret into
# substrate-sim so those calls carry the shared token instead of silently 401ing
# (carry-forward from the Phase-2 RBAC/secret review). Harmless no-op under the
# `none` demo tier (the Secret is absent → optional secretKeyRefs stay unset).
#
# ⚠ SIM-ONLY, DO NOT SHIP TO A REAL SUBSTRATE. This shared secret is a
# high-privilege, cross-org control-plane credential — exactly the authority the
# Entry-API/channel isolation spine denies the substrate side. It is acceptable
# here only because the sim is one cluster; in the real design the substrate is a
# separate cluster that reaches ONLY the public edge (orchestrator/Keycloak, over
# its org-scoped SVID) and has no network path to the internal ClusterIP registry.
# For a genuine remote substrate, do NOT distribute this secret: the substrate
# operator must reach the registry solely through the SVID-authenticated,
# org-scoped orchestrator surface (as the substrate-agent already does), or be
# issued a distinct org-scoped credential. (Phase-4 security review, LOW.)
if kubectl -n default get secret control-plane-auth >/dev/null 2>&1; then
  echo "    copying control-plane-auth Secret into ${SUBSTRATE_NS}..."
  kubectl -n default get secret control-plane-auth -o json \
    | jq '{apiVersion,kind,type,metadata:{name:"control-plane-auth",namespace:"'"$SUBSTRATE_NS"'"},data}' \
    | kubectl apply -f - >/dev/null
fi
kubectl apply -k "$REPO_ROOT/deploy/substrate"

# A re-run (e.g. a different ORG, or a re-issued leaf) rewrites the identity Secret
# but leaves the agent pod spec unchanged — so the agent would keep presenting its
# OLD leaf and stay attested under the stale org/fingerprint. Restart it so it
# picks up the current leaf. On a first run this is a harmless restart of the
# just-created DaemonSet.
echo "    restarting the substrate agent to pick up the current identity..."
kubectl -n "$SUBSTRATE_NS" rollout restart daemonset/substrate-spire-agent >/dev/null 2>&1 || true

echo "==> Waiting for the substrate components..."
kubectl -n "$SUBSTRATE_NS" rollout status daemonset/substrate-spiffe-csi-driver --timeout=120s
kubectl -n "$SUBSTRATE_NS" rollout status daemonset/substrate-spire-agent --timeout=120s
kubectl -n "$SUBSTRATE_NS" rollout status deployment/substrate-agent-operator --timeout=120s
kubectl -n "$SUBSTRATE_NS" rollout status deployment/substrate-agent --timeout=120s

echo ""
echo "Substrate sim is up for org '${ORG}'."
echo "Prove the identity loop with:  ./scripts/substrate-verify.sh ${ORG}"
