#!/usr/bin/env bash
# scripts/e2e.sh — helpers for the dashboard E2E suite.
#
# The only thing that differs between running inside the devcontainer and on a
# native host is the Kind kubeconfig (the devcontainer must point kubectl at the
# control-plane container IP). `make kubeconfig` already abstracts that, so once
# the dashboard is port-forwarded to localhost:8090 the browser tests are
# identical in both contexts.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cmd="${1:-portforward}"

case "$cmd" in
  portforward)
    # Repair kubeconfig for whichever context we're in, then forward the
    # dashboard. `exec` so Playwright's webServer can signal this process
    # directly on teardown.
    make -s -C "$REPO_ROOT" kubeconfig
    # Also forward the mobile-gateway public port (8080→localhost:8091) so the
    # mobile-ciba spec can drive /me/* + SSE directly. Backgrounded with a trap so
    # it's torn down when Playwright signals this process on teardown.
    echo "==> Port-forwarding mobile-gateway → http://localhost:8091"
    ( while true; do kubectl port-forward svc/mobile-gateway 8091:8080 || true; sleep 1; done ) &
    GW_PID=$!
    # Also forward Mailpit's HTTP API (svc/mailpit 8025) so the signup spec can
    # poll for the email-verification message and extract the verify link. Same
    # retry-loop pattern as the mobile-gateway forward above.
    echo "==> Port-forwarding mailpit API → http://localhost:8025"
    ( while true; do kubectl port-forward svc/mailpit 8025:8025 || true; sleep 1; done ) &
    MP_PID=$!
    # The dashboard forward is ALSO a retry loop (not a one-shot exec): a spec may
    # roll the dashboard mid-suite (e.g. caps.spec.ts → apply-realm.sh restarts the
    # dashboard after re-applying the realm), which drops a one-shot forward and
    # leaves localhost:8090 dead for the rest of the run (later navigations get
    # net::ERR_CONNECTION_REFUSED). Looping reconnects to the new pod within ~1s.
    # All three are backgrounded and reaped by the trap; `wait` blocks until
    # Playwright signals teardown.
    echo "==> Port-forwarding dashboard → http://localhost:8090"
    ( while true; do kubectl port-forward svc/dashboard 8090:8080 || true; sleep 1; done ) &
    DASH_PID=$!
    trap 'kill "$GW_PID" "$MP_PID" "$DASH_PID" 2>/dev/null || true' EXIT INT TERM
    wait
    ;;
  *)
    echo "usage: $0 [portforward]" >&2
    exit 1
    ;;
esac
