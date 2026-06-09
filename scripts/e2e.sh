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
    echo "==> Port-forwarding dashboard → http://localhost:8090"
    exec kubectl port-forward svc/dashboard 8090:8080
    ;;
  *)
    echo "usage: $0 [portforward]" >&2
    exit 1
    ;;
esac
