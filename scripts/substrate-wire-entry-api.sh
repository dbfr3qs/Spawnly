#!/usr/bin/env bash
# scripts/substrate-wire-entry-api.sh — wire the CP-mediated Entry API's
# delegated-admin socket for the LOCAL SIM (remote-substrate Phase 6 harness).
#
# The orchestrator's Entry API + substrate-agent bootstrap-entry provisioning drive
# SPIRE registration entries through execEntryBroker → the `spire-server entry` CLI
# over the server's ADMIN socket (cmd/orchestrator/substrate.go). That socket lives
# inside the spire-server container; the orchestrator is a DIFFERENT pod, so the two
# must share it. On kind we share it via a hostPath: repoint the server's socket dir
# at a hostPath and mount the SAME hostPath into the orchestrator, then set
# SPIRE_SERVER_SOCKET so the broker (and hence the Entry API + dispatch channel) go
# live instead of returning 503.
#
# The spire-server runs as a NON-root uid, so the hostPath socket dir must be
# world-writable BEFORE the server rolls, or it cannot create api.sock and
# CrashLoops. kubelet's DirectoryOrCreate makes it root:root 0755, so we pre-create
# it 1777 (sticky, /tmp-like) on every kind node first. If the roll fails we REVERT
# the server volume to its emptyDir default so a failed wire never leaves the
# platform's SPIRE identity issuance down.
#
# IDEMPOTENT + REVERSIBLE (scripts/substrate-down.sh SUBSTRATE_UNWIRE=1 unwires).
# The socket is a node-local Unix socket share, NOT a network listener.
#
# ⚠ LOCAL-SIM / e2e ONLY. A real deploy NEVER exposes the SPIRE server admin socket
# on a hostPath — the infra-edge track fronts the server's entry gRPC admin API
# behind mTLS, upgrading execEntryBroker to a gRPC broker behind the SAME
# substrate.EntryBroker seam with no authz change. See plans/remote-substrate/infra-edge.md.
set -euo pipefail

SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"
ORCH_NS="${ORCH_NAMESPACE:-default}"
CLUSTER="${KIND_CLUSTER:-agent-platform}"
# The spire-server chart mounts an emptyDir at this dir and the server writes
# api.sock into it (server.socket_path defaults to <dir>/api.sock).
SERVER_SOCKET_DIR="/tmp/spire-server/private"
HOSTPATH="${SUBSTRATE_SERVER_SOCKET_HOSTPATH:-/run/spire-server-socket}"
SOCKET_PATH="${HOSTPATH}/api.sock"

changed_server=0
changed_orch=0

# Pre-create the socket dir world-writable (1777) on every kind node, so the
# non-root spire-server can create its socket there once the volume is a hostPath.
precreate_hostpath() {
  if command -v kind >/dev/null 2>&1 && command -v docker >/dev/null 2>&1; then
    local n
    for n in $(kind get nodes --name "$CLUSTER" 2>/dev/null || true); do
      docker exec "$n" mkdir -p -m 1777 "$HOSTPATH" 2>/dev/null || true
      docker exec "$n" chmod 1777 "$HOSTPATH" 2>/dev/null || true
    done
  else
    echo "    WARN: kind/docker not found — ensure ${HOSTPATH} exists WORLD-WRITABLE (1777)"
    echo "          on the spire-server's node, else spire-server (non-root) can't create its socket."
  fi
}

echo "==> [1/3] Ensuring the spire-server admin socket is on a shared hostPath (${HOSTPATH})..."
cur_hostpath="$(kubectl -n "$SPIRE_NS" get statefulset spire-server -o jsonpath='{.spec.template.spec.volumes[?(@.name=="spire-server-socket")].hostPath.path}' 2>/dev/null || true)"
if [ "$cur_hostpath" != "$HOSTPATH" ]; then
  precreate_hostpath
  idx="$(kubectl -n "$SPIRE_NS" get statefulset spire-server -o json \
    | jq '.spec.template.spec.volumes | map(.name=="spire-server-socket") | index(true)')"
  [ "$idx" != "null" ] || { echo "ERROR: spire-server has no spire-server-socket volume — is SPIRE installed?" >&2; exit 1; }
  echo "    repointing spire-server volume[$idx] (spire-server-socket) → hostPath ${HOSTPATH}..."
  kubectl -n "$SPIRE_NS" patch statefulset spire-server --type=json -p "$(cat <<JSON
[
  {"op":"replace","path":"/spec/template/spec/volumes/${idx}","value":{"name":"spire-server-socket","hostPath":{"path":"${HOSTPATH}","type":"DirectoryOrCreate"}}}
]
JSON
)"
  changed_server=1
else
  echo "    already a hostPath — skipping server volume patch."
fi

if [ "$changed_server" = "1" ]; then
  echo "==> Rolling spire-server to re-create its admin socket on the hostPath..."
  kubectl -n "$SPIRE_NS" rollout restart statefulset/spire-server
  if ! kubectl -n "$SPIRE_NS" rollout status statefulset/spire-server --timeout=180s; then
    echo "ERROR: spire-server did not become ready after the hostPath repoint — REVERTING to emptyDir" >&2
    ridx="$(kubectl -n "$SPIRE_NS" get statefulset spire-server -o json | jq '.spec.template.spec.volumes | map(.name=="spire-server-socket") | index(true)')"
    [ "$ridx" != "null" ] && kubectl -n "$SPIRE_NS" patch statefulset spire-server --type=json \
      -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/volumes/${ridx}\",\"value\":{\"name\":\"spire-server-socket\",\"emptyDir\":{}}}]" || true
    kubectl -n "$SPIRE_NS" rollout status statefulset/spire-server --timeout=180s || true
    echo "REVERTED — SPIRE restored. Pre-create ${HOSTPATH} world-writable on the server's node and retry." >&2
    exit 1
  fi
fi

echo "==> [2/3] Mounting the shared socket into the orchestrator + setting SPIRE_SERVER_SOCKET..."
have_vol="$(kubectl -n "$ORCH_NS" get deployment orchestrator -o jsonpath='{.spec.template.spec.volumes[?(@.name=="spire-server-socket")].name}' 2>/dev/null || true)"
if [ "$have_vol" != "spire-server-socket" ]; then
  kubectl -n "$ORCH_NS" patch deployment orchestrator --type=strategic -p "$(cat <<JSON
{
  "spec": { "template": { "spec": {
    "volumes": [
      { "name": "spire-server-socket", "hostPath": { "path": "${HOSTPATH}", "type": "DirectoryOrCreate" } }
    ],
    "containers": [
      { "name": "orchestrator", "volumeMounts": [
          { "name": "spire-server-socket", "mountPath": "${HOSTPATH}" } ] }
    ]
  } } }
}
JSON
)"
  changed_orch=1
else
  echo "    orchestrator already mounts the shared socket — skipping volume patch."
fi

cur_sock="$(kubectl -n "$ORCH_NS" get deployment orchestrator -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="SPIRE_SERVER_SOCKET")].value}' 2>/dev/null || true)"
if [ "$cur_sock" != "$SOCKET_PATH" ]; then
  kubectl -n "$ORCH_NS" set env deployment/orchestrator "SPIRE_SERVER_SOCKET=${SOCKET_PATH}"
  changed_orch=1
else
  echo "    SPIRE_SERVER_SOCKET already set — skipping."
fi

if [ "$changed_orch" = "1" ]; then
  echo "==> Rolling the orchestrator to pick up the Entry API broker..."
  kubectl -n "$ORCH_NS" rollout status deployment/orchestrator --timeout=180s
fi

# [3/3] The hostPath socket is node-local: the Entry API only works if the
# orchestrator and spire-server land on the SAME node. Warn loudly (don't fail —
# runtime is fail-closed) with remediation if kind scheduled them apart.
echo "==> [3/3] Verifying orchestrator + spire-server co-location (node-local socket)..."
srv_node="$(kubectl -n "$SPIRE_NS" get pod spire-server-0 -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)"
orch_node="$(kubectl -n "$ORCH_NS" get pod -l app=orchestrator -o jsonpath='{.items[0].spec.nodeName}' 2>/dev/null || true)"
if [ -n "$srv_node" ] && [ -n "$orch_node" ] && [ "$srv_node" != "$orch_node" ]; then
  echo "    WARN: orchestrator (${orch_node}) and spire-server (${srv_node}) are on DIFFERENT nodes."
  echo "          The Entry API socket is node-local, so it will NOT be reachable. Co-locate them"
  echo "          (e.g. cordon other nodes and rollout restart deploy/orchestrator) and re-run."
else
  echo "    co-located on ${orch_node:-<node>}."
fi

echo ""
echo "Entry API delegated-admin socket wired (${SOCKET_PATH}). The orchestrator's"
echo "Entry API + dispatch channel are now LIVE — connect a substrate to activate it."
