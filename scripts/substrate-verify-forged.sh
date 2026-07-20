#!/usr/bin/env bash
# scripts/substrate-verify-forged.sh — AC 1 negative: a substrate leaf signed by
# a DIFFERENT/forged CA (one the hosted SPIRE server does NOT trust) must fail
# x509pop node attestation, so the substrate never activates. Even though the
# forged leaf carries the SAME CommonName (org), the server rejects it because it
# does not chain to the trusted dev CA.
#
# Called by scripts/substrate-verify.sh. Exits 0 iff attestation was rejected.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORG="${1:-acme}"
SUBSTRATE_NS="${SUBSTRATE_NAMESPACE:-substrate-sim}"
SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"
TD="${SPIRE_TRUST_DOMAIN:-cluster.local}"
POD="substrate-forged-agent"
SERVER_POD="spire-server-0"
SERVER="kubectl -n ${SPIRE_NS} exec ${SERVER_POD} -c spire-server -- /opt/spire/bin/spire-server"

# The authoritative signal: the set of x509pop nodes the SERVER has attested. A
# forged leaf that WRONGLY attested adds a NEW node (its fingerprint differs from
# the trusted agent's); a correctly-rejected one adds none. This is independent of
# how the agent happens to word its logs.
list_nodes() { $SERVER agent list 2>/dev/null | grep -oE "spiffe://[^ ]*/spire/agent/x509pop/[^ ]+" | sort -u || true; }

TMP="$(mktemp -d)"; chmod 700 "$TMP"
cleanup() {
  rm -rf "$TMP"
  kubectl -n "$SUBSTRATE_NS" delete pod "$POD" --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true
  kubectl -n "$SUBSTRATE_NS" delete secret substrate-forged-identity --ignore-not-found >/dev/null 2>&1 || true
  kubectl -n "$SUBSTRATE_NS" delete configmap substrate-forged-agent --ignore-not-found >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Forge a CA the server does NOT trust, and sign a same-org leaf off it.
(cd "$REPO_ROOT" && go run ./cmd/substrate-certgen -mode ca \
   -cert-out "$TMP/forge-ca.crt" -key-out "$TMP/forge-ca.key" >/dev/null 2>&1)
(cd "$REPO_ROOT" && go run ./cmd/substrate-certgen -mode leaf -org "$ORG" -trust-domain "$TD" \
   -ca-cert "$TMP/forge-ca.crt" -ca-key "$TMP/forge-ca.key" \
   -cert-out "$TMP/forge.crt" -key-out "$TMP/forge.key" >/dev/null 2>&1)

kubectl -n "$SUBSTRATE_NS" create secret generic substrate-forged-identity \
  --from-file=svid.crt="$TMP/forge.crt" --from-file=svid.key="$TMP/forge.key" >/dev/null

# Minimal one-off agent: distinct socket dir + health port, no hostNetwork — it
# only needs to reach the server to (fail to) attest. Reuses the real server
# trust bundle (spire-bundle) so the ONLY difference from a valid agent is the
# untrusted signing CA.
kubectl -n "$SUBSTRATE_NS" create configmap substrate-forged-agent --from-literal=agent.conf='{
  "agent": {
    "data_dir": "/var/lib/spire",
    "log_level": "DEBUG",
    "server_address": "spire-server.spire-system",
    "server_port": "443",
    "socket_path": "/run/spire/forged-sockets/spire-agent.sock",
    "trust_bundle_format": "spiffe",
    "trust_bundle_path": "/run/spire/bundle/bundle.spiffe",
    "trust_domain": "'"$TD"'"
  },
  "health_checks": {"bind_address":"0.0.0.0","bind_port":"9991","listener_enabled":true,"live_path":"/live","ready_path":"/ready"},
  "plugins": {
    "KeyManager": [{"memory": {"plugin_data": null}}],
    "NodeAttestor": [{"x509pop": {"plugin_data": {"certificate_path":"/run/spire/identity/svid.crt","private_key_path":"/run/spire/identity/svid.key"}}}],
    "WorkloadAttestor": [{"k8s": {"plugin_data": {"skip_kubelet_verification": true}}}]
  }
}' >/dev/null

# Snapshot the attested-node set BEFORE the forged agent can attempt attestation.
BEFORE="$(list_nodes)"

kubectl -n "$SUBSTRATE_NS" delete pod "$POD" --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: ${SUBSTRATE_NS}
spec:
  restartPolicy: Never
  containers:
    - name: spire-agent
      image: ghcr.io/spiffe/spire-agent:1.15.1
      args: ["-config", "/opt/spire/conf/agent/agent.conf"]
      volumeMounts:
        - {name: spire-config, mountPath: /opt/spire/conf/agent, readOnly: true}
        - {name: spire-identity, mountPath: /run/spire/identity, readOnly: true}
        - {name: spire-bundle, mountPath: /run/spire/bundle, readOnly: true}
        - {name: spire-data, mountPath: /var/lib/spire}
  volumes:
    - {name: spire-config, configMap: {name: substrate-forged-agent}}
    - {name: spire-identity, secret: {secretName: substrate-forged-identity}}
    - {name: spire-bundle, configMap: {name: spire-bundle}}
    - {name: spire-data, emptyDir: {}}
YAML

kubectl -n "$SUBSTRATE_NS" wait --for=condition=Ready "pod/${POD}" --timeout=60s >/dev/null 2>&1 || true

# Give it time to attempt attestation. The AUTHORITATIVE gate is the server's node
# set: if a NEW x509pop node appears (one not in BEFORE), the forgery was accepted
# — that is a hard failure. A tightened log match is only a secondary early-exit
# for a cleanly-rejected agent; it can never turn a real attestation into a pass.
echo "    forged agent deployed; watching the server's attested-node set..."
for _ in $(seq 1 20); do
  NEW="$(comm -13 <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$(list_nodes)"))"
  if [ -n "$NEW" ]; then
    echo "    UNEXPECTED: forged agent ATTESTED — new node(s):" >&2
    printf '      %s\n' "$NEW" >&2
    exit 1
  fi
  LOGS="$(kubectl -n "$SUBSTRATE_NS" logs "$POD" 2>/dev/null || true)"
  if printf '%s' "$LOGS" | grep -qiE "certificate signed by unknown authority|failed to (get|receive|obtain).*(svid|attest)|attestation.*(fail|error)|could not get.*bundle"; then
    echo "    forged agent logged an attestation failure and added no node — rejected."
    exit 0
  fi
  sleep 3
done

# Fell through the window: the decisive check is that NO new node was attested.
if [ -z "$(comm -13 <(printf '%s\n' "$BEFORE") <(printf '%s\n' "$(list_nodes)"))" ]; then
  echo "    no new x509pop node was attested for the forged leaf — rejected."
  exit 0
fi
echo "    forged agent produced a new node — NOT rejected" >&2
exit 1
