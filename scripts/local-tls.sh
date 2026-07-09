#!/usr/bin/env bash
# scripts/local-tls.sh — local full-fidelity TLS mimic for the org-subdomains
# feature (Phase 3). Stands up HTTPS for the apex + satellites over the running
# kind stack so the whole subdomain flow works in a browser:
#
#   https://localtest.me         → apex (Global/picker/signup + Keycloak proxy)
#   https://<slug>.localtest.me  → org satellite (scoped dashboard, own /callback)
#
# This is a MODE-SWITCH, not a simultaneous overlay. Keycloak's issuer is a single
# string, so the cluster serves EITHER the default localhost:8090 origin OR the
# https://localtest.me origin at a time. `up` flips into TLS mode (mirroring the
# AWS deploy.sh 5c wiring); `down` restores the manifest defaults. Both idempotent.
#
#   ./scripts/local-tls.sh up     # or: make tls-mimic
#   ./scripts/local-tls.sh down   # or: make tls-mimic-down
#
# MUTUALLY EXCLUSIVE with `make dash` / `make e2e` (the default single-origin
# mode). Run `down` before returning to the default dev loop.
#
# Zero host installs: TLS terminates in a Caddy docker container using its own
# internal CA (deploy/local-tls/Caddyfile). Browsers need to trust that CA (this
# script exports the root to deploy/local-tls/certs/root.crt) or ignore the cert
# warning; Phase 4's Playwright suite uses ignoreHTTPSErrors.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TLS_DIR="$REPO_ROOT/deploy/local-tls"
CADDY_IMAGE="caddy:2"
CADDY_NAME="spawnly-tls-mimic"
CADDY_VOLUME="spawnly-tls-mimic-data"   # persists the internal-CA root across cycles
PF_PID_FILE="$TLS_DIR/.port-forward.pid"

APEX="localtest.me"
APEX_ORIGIN="https://${APEX}"
REALM_ISSUER="${APEX_ORIGIN}/realms/spawnly"

# The exact resource-server set whose expected issuer flips (mirrors
# spawnly-infra deploy/aws/deploy.sh section 5c). Some (travel-tools,
# mobile-gateway) may not be deployed on a lean cluster — tolerated below.
RESOURCE_SERVERS=(orchestrator sample-api-a sample-api-b travel-tools mobile-gateway)

# Manifest-default issuer for the DEFAULT (localhost) mode. NOTE: on kind the
# browser reaches Keycloak through the dashboard's apex proxy, so Keycloak stamps
# iss=http://localhost:8090/realms/spawnly and the resource servers validate that
# (deploy/manifests/*.yaml). It is NOT keycloak:8080 (that is IS_JWKS_URL, the
# backchannel). `down` therefore restores localhost:8090, matching the manifests.
LOCAL_ORIGIN="http://localhost:8090"
LOCAL_ISSUER="${LOCAL_ORIGIN}/realms/spawnly"

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

require_cmds() {
  local c
  for c in "$@"; do
    command -v "$c" >/dev/null 2>&1 || die "'$c' not found on PATH."
  done
}

# port_open PORT — true if something is already listening on 127.0.0.1:PORT.
port_open() {
  [[ "$1" =~ ^[0-9]+$ ]] || die "port_open: non-numeric port '$1'"
  PORT="$1" timeout 1 bash -c 'exec 3<>/dev/tcp/127.0.0.1/$PORT' 2>/dev/null
}

# ── dashboard port-forward (localhost:8090) ───────────────────────────────────
# Reuses the loop shape from the Makefile `port-forward` target so it survives
# brief drops. Owned by this script during TLS mode (down kills it).
ensure_port_forward() {
  if port_open 8090; then
    # Only reuse it if it IS the dashboard forward — otherwise Caddy would proxy
    # to a foreign backend on 8090. Fail loudly rather than mis-proxy.
    if pgrep -f '[k]ubectl port-forward svc/dashboard 8090' >/dev/null 2>&1; then
      log "dashboard port-forward already up (localhost:8090)"
      return
    fi
    die "localhost:8090 is in use by a non-dashboard process — free it, then retry."
  fi
  log "starting dashboard port-forward (localhost:8090)"
  setsid bash -c \
    'while true; do kubectl port-forward svc/dashboard 8090:8080 >/dev/null 2>&1; sleep 1; done' \
    >/dev/null 2>&1 &
  echo $! >"$PF_PID_FILE"
  for _ in $(seq 1 30); do
    port_open 8090 && { log "port-forward ready"; return; }
    sleep 1
  done
  die "dashboard port-forward did not come up on localhost:8090 (is the cluster bootstrapped?)"
}

stop_port_forward() {
  # The reconnect loop's own cmdline contains this pattern (it wraps the kubectl
  # invocation), so a single pkill -f reaps both the loop wrapper and its child —
  # no stale-pidfile kill (which could signal a recycled, unrelated PID).
  pkill -f 'kubectl port-forward svc/dashboard 8090:8080' 2>/dev/null || true
  rm -f "$PF_PID_FILE"
}

# ── Caddy TLS terminator ──────────────────────────────────────────────────────
# --network host: Caddy binds host :443 directly and reaches the port-forward at
# localhost:8090 with no host.docker.internal plumbing (this is a Linux
# devcontainer). On Docker Desktop / macOS, swap to
#   -p 443:443 --add-host=host.docker.internal:host-gateway
# and set DASH_UPSTREAM=host.docker.internal:8090. Root in the container binds the
# privileged :443.
#
# The Caddyfile is `docker cp`d in (NOT bind-mounted): the devcontainer's Docker
# daemon can't see the workspace path, so a `-v file:file` mount silently creates
# an empty dir (same reason the mvn tests use docker cp). Internal-CA state lives
# in a named volume so the browser-trusted root survives across up/down cycles;
# the root is exported to deploy/local-tls/certs/root.crt (gitignored) for trust.
start_caddy() {
  docker rm -f "$CADDY_NAME" >/dev/null 2>&1 || true
  log "starting Caddy TLS terminator ($CADDY_IMAGE) on :443"
  # Least privilege: drop all caps, keep only NET_BIND_SERVICE for the :443 bind.
  docker create --name "$CADDY_NAME" \
    --network host \
    --cap-drop ALL --cap-add NET_BIND_SERVICE \
    -v "$CADDY_VOLUME:/data" \
    "$CADDY_IMAGE" >/dev/null
  docker cp "$TLS_DIR/Caddyfile" "$CADDY_NAME:/etc/caddy/Caddyfile" >/dev/null
  docker start "$CADDY_NAME" >/dev/null
  # Confirm Caddy stayed up over a few seconds (a bad config exits within ~1-2s;
  # re-checking rather than a single fixed sleep catches a slightly slower crash).
  local running=
  for _ in $(seq 1 3); do
    if [ "$(docker inspect -f '{{.State.Running}}' "$CADDY_NAME" 2>/dev/null || echo false)" = "true" ]; then
      running=1
    else
      running=   # exited — surface it
      break
    fi
    sleep 1
  done
  if [ -z "$running" ]; then
    warn "Caddy container is not running — logs:"
    docker logs "$CADDY_NAME" 2>&1 | sed 's/^/    /' >&2 || true
    die "Caddy failed to start (see logs above)."
  fi
  export_root_ca
  log "Caddy up on https://${APEX} (:443)"
}

# Export Caddy's internal-CA root so a human can trust it once (best-effort).
export_root_ca() {
  mkdir -p "$TLS_DIR/certs"
  local root=/data/caddy/pki/authorities/local/root.crt
  for _ in $(seq 1 10); do
    if docker cp "$CADDY_NAME:$root" "$TLS_DIR/certs/root.crt" >/dev/null 2>&1; then
      return
    fi
    sleep 1   # the CA is minted lazily on first TLS handshake / startup
  done
  warn "could not export the Caddy root CA yet (it is minted on first use); re-run or accept the browser warning."
}

stop_caddy() {
  if docker ps -a --format '{{.Names}}' | grep -qx "$CADDY_NAME"; then
    log "stopping Caddy TLS terminator"
    docker rm -f "$CADDY_NAME" >/dev/null 2>&1 || true
  fi
}

# ── cluster issuer-mode flip ──────────────────────────────────────────────────
# present_resource_servers — the RESOURCE_SERVERS that actually exist on this
# cluster (a lean cluster may omit travel-tools / mobile-gateway).
present_resource_servers() {
  local d
  for d in "${RESOURCE_SERVERS[@]}"; do
    kubectl get "deployment/$d" >/dev/null 2>&1 && printf '%s\n' "$d"
  done
}

# set_resource_issuers ISSUER — flip IS_ISSUER on every PRESENT resource server.
# A not-deployed server is skipped up front; a real kubectl error is NOT swallowed
# (set -e surfaces it) so we never silently half-flip and leak wrong-issuer 401s.
set_resource_issuers() {
  local issuer="$1" d
  for d in $(present_resource_servers); do
    kubectl set env "deployment/$d" IS_ISSUER="$issuer" >/dev/null
  done
}

# wait_resource_servers — block until each present resource server finishes the
# issuer-flip rollout, so `up`/`down` don't return while old-issuer pods serve.
wait_resource_servers() {
  local d
  for d in $(present_resource_servers); do
    kubectl rollout status "deployment/$d" --timeout=120s >/dev/null \
      || warn "$d rollout did not complete in time — check it manually"
  done
}

# apply_realm ORIGIN — re-apply the Keycloak realm for the given dashboard origin
# and reseed templates + demo orgs (apply-realm.sh reseeds at the end, which
# re-registers the *.localtest.me satellite redirect URIs after Terraform resets
# the client — the Phase 2 reseed-after-apply invariant). apply-realm.sh honors a
# pre-set TF_VAR_dashboard_origin (defaults to localhost:8090 when unset).
apply_realm() {
  local origin="$1"
  log "re-applying realm (dashboard_origin=$origin) + reseeding orgs"
  TF_VAR_dashboard_origin="$origin" "$REPO_ROOT/scripts/apply-realm.sh"
}

# ── up ────────────────────────────────────────────────────────────────────────
cmd_up() {
  require_cmds docker kubectl make terraform curl
  # A mid-flip failure leaves the cluster partly switched — tell the operator how
  # to get back to a known-good state.
  trap 'warn "up failed mid-flip — run: make tls-mimic-down  (restores the default mode)"' ERR
  log "TLS mimic UP — flipping cluster into apex=${APEX_ORIGIN} mode"

  # (a) Load the Phase 1/2 dashboard + registry images into kind. The running
  #     pods may be pre-Phase-1 images; the set env / rollout below picks these up
  #     (imagePullPolicy: Never, tag :latest → a restart re-reads the loaded image).
  log "building + loading Phase 1/2 dashboard + registry images into kind"
  make -C "$REPO_ROOT" reload-dashboard reload-registry

  # (b) dashboard port-forward that Caddy proxies to.
  ensure_port_forward

  # (c) TLS terminator.
  start_caddy

  # (d) Flip the cluster (mirrors spawnly-infra deploy/aws/deploy.sh 5c/6b):
  log "wiring Keycloak frontend origin + OIDC + resource-server issuers"
  kubectl set env deployment/keycloak  KC_HOSTNAME="$APEX_ORIGIN" >/dev/null
  kubectl set env deployment/dashboard OIDC_AUTHORITY="$APEX_ORIGIN" APEX_DOMAIN="$APEX" >/dev/null
  set_resource_issuers "$REALM_ISSUER"
  kubectl set env deployment/registry  ORG_APEX_DOMAIN="$APEX" >/dev/null
  # Guarantee the freshly-loaded registry image + ORG_APEX_DOMAIN are live even on
  # an idempotent re-up (set env is a no-op when unchanged → no rollout). The
  # dashboard is restarted by apply-realm.sh below.
  kubectl rollout restart deployment/registry >/dev/null
  kubectl rollout status  deployment/registry --timeout=120s

  # (e) Realm re-apply for the apex origin + reseed (registers satellite URIs).
  apply_realm "$APEX_ORIGIN"

  # Don't declare victory while sample-api-a/b, travel-tools, mobile-gateway are
  # still rolling to the new issuer (apply_realm only waits orchestrator+dashboard).
  wait_resource_servers

  cat <<EOF

TLS mimic is UP (apex=${APEX_ORIGIN}).

  Browser:   ${APEX_ORIGIN}                (Global / org picker / signup)
             https://acme.${APEX}          (acme satellite — alice admin, viewer member)
             https://globex.${APEX}        (globex satellite — bob admin)

  Caddy uses its internal CA. Trust the exported root once:
    ${TLS_DIR}/certs/root.crt
  or accept the browser warning. Phase 4 Playwright uses ignoreHTTPSErrors.

  This is a MODE-SWITCH — the default localhost:8090 loop (make dash / make e2e)
  is OFF while this is up. Return with:  make tls-mimic-down
EOF
}

# ── down ──────────────────────────────────────────────────────────────────────
cmd_down() {
  require_cmds docker kubectl terraform curl
  # Always drop the port-forward we own, even if the realm re-apply flakes (it
  # uses its own :18099 forward, so 8090 isn't needed past this point).
  trap stop_port_forward EXIT
  log "TLS mimic DOWN — restoring default localhost:8090 mode"

  stop_caddy

  # Restore the manifest-default env (single-origin mode).
  log "restoring Keycloak frontend origin + OIDC + resource-server issuers"
  kubectl set env deployment/keycloak  KC_HOSTNAME="$LOCAL_ORIGIN" >/dev/null
  kubectl set env deployment/dashboard OIDC_AUTHORITY="$LOCAL_ORIGIN" APEX_DOMAIN="" >/dev/null
  set_resource_issuers "$LOCAL_ISSUER"
  kubectl set env deployment/registry  ORG_APEX_DOMAIN="" >/dev/null
  kubectl rollout restart deployment/registry >/dev/null
  kubectl rollout status  deployment/registry --timeout=120s

  # Wait out the resource-server issuer-flip rollouts before declaring default
  # mode restored (the port-forward is dropped by the EXIT trap above).
  wait_resource_servers

  cat <<EOF

TLS mimic is DOWN — cluster restored to the default localhost:8090 mode.
  Resume the default dev loop with:  make dash   (and make e2e for the suite).
EOF
}

main() {
  case "${1:-}" in
    up)   cmd_up ;;
    down) cmd_down ;;
    *)    die "usage: $0 {up|down}" ;;
  esac
}

main "$@"
