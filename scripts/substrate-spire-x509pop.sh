#!/usr/bin/env bash
# scripts/substrate-spire-x509pop.sh — enable the x509pop node attestor on the
# hosted SPIRE server, ALONGSIDE the platform's k8s_psat, so a substrate SPIRE
# agent can attest with a CP-issued org-bound leaf (external_pki, dev CA).
#
# WHY a post-render patch and not chart values: the spire-server chart 0.29.0's
# native nodeAttestor.x509POP only supports "spiffe" mode and never emits
# ca_bundle_path — it cannot do external_pki with a dev CA (see
# deploy/spire/values.yaml). So this script injects the plugin block into the
# rendered spire-server ConfigMap and mounts the dev CA cert into the server.
#
# IDEMPOTENT: run it after `helm ... spire` (make spire / bootstrap) and any
# number of times — it re-derives the same config, adds the x509pop attestor
# exactly once, and only rolls the server when something actually changed.
# k8s_psat and every platform pod are left untouched.
#
# ⚠ LOCAL-SIM / e2e only. The real edge-exposed server + real CA are the
# spawnly-infra track; this is the design stub it replaces.
set -euo pipefail

NS="${SPIRE_NAMESPACE:-spire-system}"
CA_SECRET="${SUBSTRATE_CA_SECRET:-substrate-dev-ca}"
CA_MOUNT_DIR="/run/spire/x509pop"
CA_BUNDLE_PATH="${CA_MOUNT_DIR}/ca.pem"
# Must match internal/substrate/devca.NodeAttestorPathTemplate.
# MUST match devca.NodeAttestorPathTemplate, and must NOT begin with
# "/spire/agent": SPIRE already roots attested node ids at
# spiffe://<td>/spire/agent and appends the rendered template, so a prefix here
# yields a DOUBLED id that never matches devca.NodeSpiffePathPrefix.
AGENT_PATH_TEMPLATE='/x509pop/org/{{ .Subject.CommonName }}/{{ .Fingerprint }}'

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# 1. Ensure the dev CA Secret exists.
"$REPO_ROOT/scripts/substrate-ca.sh"

changed=0

# 2. Mount ONLY the CA cert (ca.crt -> ca.pem) into the spire-server container.
#    Strategic-merge patch: volumes/volumeMounts merge by name, so re-applying is
#    a no-op. The CA private key (ca.key) is deliberately NOT projected.
if ! kubectl -n "$NS" get statefulset spire-server \
     -o jsonpath='{.spec.template.spec.volumes[?(@.name=="x509pop-ca")].name}' 2>/dev/null | grep -q x509pop-ca; then
  echo "==> Mounting dev CA cert into spire-server at ${CA_MOUNT_DIR}..."
  kubectl -n "$NS" patch statefulset spire-server --type=strategic -p "$(cat <<JSON
{
  "spec": { "template": { "spec": {
    "volumes": [
      { "name": "x509pop-ca", "secret": {
          "secretName": "${CA_SECRET}",
          "items": [ { "key": "ca.crt", "path": "ca.pem" } ] } }
    ],
    "containers": [
      { "name": "spire-server", "volumeMounts": [
          { "name": "x509pop-ca", "mountPath": "${CA_MOUNT_DIR}", "readOnly": true } ] }
    ]
  } } }
}
JSON
)"
  changed=1
else
  echo "==> spire-server already mounts the dev CA — skipping volume patch."
fi

# 3. Inject the x509pop NodeAttestor into the server ConfigMap (server.conf JSON),
#    exactly once (idempotent jq: drop any existing x509pop, then append).
echo "==> Ensuring x509pop NodeAttestor in the spire-server ConfigMap..."
# ORCH_ADMIN_ID is the orchestrator's SPIFFE id trusted as a delegated admin for
# the Entry API's gRPC path (the infra-track upgrade of the local-sim CLI broker).
# admin_ids only gates who may call the SPIRE server ADMIN API; the per-entry
# org authorization lives in internal/substrate.EntryService, never here. Adding
# it is harmless for the CLI-over-socket local-sim broker (which is admin by
# virtue of socket access) and readies the server for the gRPC broker.
ORCH_ADMIN_ID="${ORCH_ADMIN_ID:-spiffe://cluster.local/agent/orchestrator}"
cur="$(kubectl -n "$NS" get cm spire-server -o jsonpath='{.data.server\.conf}')"
new="$(printf '%s' "$cur" | jq \
  --arg ca "$CA_BUNDLE_PATH" --arg tmpl "$AGENT_PATH_TEMPLATE" --arg admin "$ORCH_ADMIN_ID" \
  '.plugins.NodeAttestor = ([.plugins.NodeAttestor[] | select(has("x509pop") | not)]
     + [{"x509pop":{"plugin_data":{"ca_bundle_path":$ca,"agent_path_template":$tmpl}}}])
   | .server.admin_ids = (((.server.admin_ids // []) - [$admin]) + [$admin])')"

if [ "$(printf '%s' "$cur" | jq -S .)" != "$(printf '%s' "$new" | jq -S .)" ]; then
  # Merge-patch the single data key with the new JSON string.
  kubectl -n "$NS" patch cm spire-server --type=merge \
    -p "$(jq -n --arg c "$new" '{data:{"server.conf":$c}}')"
  changed=1
else
  echo "    x509pop already present — ConfigMap unchanged."
fi

# 4. Roll the server only if we changed something, then wait for it back.
if [ "$changed" = "1" ]; then
  echo "==> Rolling spire-server to load x509pop + the CA mount..."
  kubectl -n "$NS" rollout restart statefulset/spire-server
  kubectl -n "$NS" rollout status statefulset/spire-server --timeout=180s
else
  echo "==> x509pop already enabled — nothing to roll."
fi

echo "Done. spire-server now accepts x509pop nodes under the org path (alongside k8s_psat)."
