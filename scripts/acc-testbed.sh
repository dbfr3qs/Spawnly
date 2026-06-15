#!/usr/bin/env bash
# acc-testbed.sh {up|down} — a lightweight registry testbed for the Terraform
# provider's acceptance + parity tests.
#
# It runs only what those tests actually exercise: an in-memory SpiceDB plus the
# registry under shared-secret control-plane auth. No SPIRE, identity-server,
# kind, or agents — the provider only talks to the registry's template
# control-plane API, and the registry boots fine with REGISTRANT_VERIFIER=mtls
# (which the template endpoints never invoke).
#
# Networking: the registry connects to SpiceDB by the container's IP (not a
# published port). That works identically from this devcontainer and from a
# native CI runner, whereas localhost+published-port does not reach containers
# from inside a devcontainer.
set -euo pipefail

# The registry module requires Go >= 1.25 (root go.mod / go.work). Force the
# auto toolchain so an older base `go` self-upgrades rather than compiling with
# a stale std (e.g. crypto/pbkdf2, added in 1.24, used by go-jose).
export GOTOOLCHAIN=auto

# Drop any inherited GOROOT. VS Code's Go extension (and stale interactive
# shells) often export GOROOT pointing at a downloaded toolchain that no longer
# matches the `go` binary on PATH, which makes the build fail with
# `compile: version "goX" does not match go tool version "goY"`. Letting `go`
# derive GOROOT from its own binary avoids that skew.
unset GOROOT

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STATE_DIR="${SPAWNLY_ACC_STATE_DIR:-/tmp/spawnly-acc-testbed}"
SPICEDB_NAME="spawnly-acc-spicedb"
SPICEDB_IMAGE="${SPICEDB_IMAGE:-authzed/spicedb:latest}"
SPICEDB_PSK="poc-secret"
TOKEN="${SPAWNLY_ACC_TOKEN:-acc-test-token}"
ENDPOINT="http://localhost:8080" # the registry binds :8080

REG_BIN="$STATE_DIR/registry"
REG_LOG="$STATE_DIR/registry.log"
REG_PID="$STATE_DIR/registry.pid"
ENV_FILE="$STATE_DIR/env"

up() {
  mkdir -p "$STATE_DIR"

  if [ "$(curl -s -o /dev/null -w '%{http_code}' "$ENDPOINT/v1/schema" 2>/dev/null)" = "200" ]; then
    echo "ERROR: something is already serving $ENDPOINT (a dev-cluster port-forward?). Stop it first." >&2
    exit 1
  fi

  echo "==> starting in-memory SpiceDB ($SPICEDB_IMAGE)"
  docker rm -f "$SPICEDB_NAME" >/dev/null 2>&1 || true
  docker run -d --name "$SPICEDB_NAME" "$SPICEDB_IMAGE" \
    serve --grpc-preshared-key "$SPICEDB_PSK" --datastore-engine memory >/dev/null

  local ip=""
  for _ in $(seq 1 30); do
    ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$SPICEDB_NAME" 2>/dev/null || true)"
    if [ -n "$ip" ] && timeout 2 bash -c "cat </dev/null >/dev/tcp/$ip/50051" 2>/dev/null; then
      break
    fi
    sleep 1
  done
  [ -n "$ip" ] || { echo "SpiceDB did not become reachable" >&2; exit 1; }
  echo "    SpiceDB reachable at $ip:50051"

  echo "==> building registry"
  if ! ( cd "$REPO_ROOT" && go build -o "$REG_BIN" ./cmd/registry ); then
    echo "ERROR: registry build failed. It requires Go >= 1.25 (root go.mod)." >&2
    echo "       Your go: $(go version); GOTOOLCHAIN=$(go env GOTOOLCHAIN)." >&2
    echo "       Upgrade Go to >= 1.25, or ensure GOTOOLCHAIN=auto with network access." >&2
    exit 1
  fi
  : > "$STATE_DIR/dummy-ca.pem" # mtls verifier only needs the path set; never read at boot

  echo "==> starting registry (shared-secret, no SPIRE)"
  nohup env \
    SPICEDB_ENDPOINT="$ip:50051" SPICEDB_PSK="$SPICEDB_PSK" \
    REGISTRANT_VERIFIER=mtls MTLS_CLIENT_CA_PATH="$STATE_DIR/dummy-ca.pem" \
    CONTROL_PLANE_AUTH=shared-secret CONTROL_PLANE_TOKEN="$TOKEN" \
    "$REG_BIN" >"$REG_LOG" 2>&1 &
  echo $! > "$REG_PID"
  disown 2>/dev/null || true

  local code=""
  for _ in $(seq 1 30); do
    code="$(curl -s -o /dev/null -w '%{http_code}' "$ENDPOINT/v1/schema" 2>/dev/null || true)"
    [ "$code" = "200" ] && break
    sleep 1
  done
  if [ "$code" != "200" ]; then
    echo "registry did not become healthy; last log lines:" >&2
    tail -20 "$REG_LOG" >&2
    exit 1
  fi

  cat > "$ENV_FILE" <<EOF
export SPAWNLY_ENDPOINT=$ENDPOINT
export SPAWNLY_TOKEN=$TOKEN
EOF
  echo "==> testbed up at $ENDPOINT  (source $ENV_FILE for SPAWNLY_ENDPOINT/SPAWNLY_TOKEN)"
}

down() {
  if [ -f "$REG_PID" ]; then
    kill "$(cat "$REG_PID")" 2>/dev/null || true
  fi
  docker rm -f "$SPICEDB_NAME" >/dev/null 2>&1 || true
  rm -rf "$STATE_DIR"
  echo "==> testbed down"
}

case "${1:-}" in
  up) up ;;
  down) down ;;
  *) echo "usage: $0 {up|down}" >&2; exit 2 ;;
esac
