#!/usr/bin/env bash
# scripts/seed.sh — seed agent templates into a running registry.
# Run after redeploying the registry (which resets its in-memory store).
#
# Templates are co-located with their agents as `template.json` files
# (agents/<type>/template.json, including the worker at agents/go-worker/).
# To add a new agent type, drop a template.json next to it — no edits here.
set -euo pipefail

# Resolve globs relative to the repo root regardless of where we're invoked from.
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Discover every co-located template. The glob is depth-bounded
# (agents/<type>/) so node_modules/dist can't match.
templates=(agents/*/template.json)

# Filter to files that actually exist (an unmatched glob stays literal).
found=()
for f in "${templates[@]}"; do
  [ -f "$f" ] && found+=("$f")
done
if [ "${#found[@]}" -eq 0 ]; then
  echo "ERROR: no template.json files found — nothing to seed." >&2
  exit 1
fi

# Template writes are a control-plane operation. When the cluster was bootstrapped
# with shared-secret enforcement the `control-plane-auth` Secret holds the token;
# present it as a bearer. On an open ("none") cluster the Secret is absent, the
# token is empty, and we send no auth header — seeding works either way.
CP_TOKEN=$(kubectl get secret control-plane-auth -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
auth_header=()
[ -n "$CP_TOKEN" ] && auth_header=(-H "Authorization: Bearer ${CP_TOKEN}")

echo "==> Port-forwarding registry..."
kubectl port-forward svc/registry 18080:8080 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT
sleep 2

echo "==> Seeding templates..."
for f in "${found[@]}"; do
  agent_type=$(jq -r .agentType "$f")
  curl -sf -X POST http://localhost:18080/v1/templates \
    -H 'Content-Type: application/json' \
    "${auth_header[@]}" \
    --data-binary @"$f" >/dev/null
  echo "  ${agent_type}"
done

echo ""
echo "Done. All templates seeded."
