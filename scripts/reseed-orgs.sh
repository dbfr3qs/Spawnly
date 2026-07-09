#!/usr/bin/env bash
# scripts/reseed-orgs.sh — seed the demo organizations into a running registry.
#
# Creates two orgs through the registry's control-plane onboarding API, which
# dual-writes Keycloak Organizations + SpiceDB tuples together (never edit org
# membership directly in the Keycloak admin console — see
# deploy/keycloak/README.md):
#
#   acme   — alice = org admin, viewer = member   (the primary demo org)
#   globex — bob   = org admin                      (cross-org isolation)
#
# Every call is idempotent (org create converges; member add is Keycloak-409 +
# SpiceDB TOUCH tolerant), so this is safe to re-run. Run after the realm is
# applied (the org-admin client must exist) and the registry is up.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

# Org mutations are a control-plane operation; present the shared-secret bearer
# when the cluster was bootstrapped with enforcement (open clusters have no
# Secret and send no header).
CP_TOKEN=$(kubectl get secret control-plane-auth -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
auth_header=()
[ -n "$CP_TOKEN" ] && auth_header=(-H "Authorization: Bearer ${CP_TOKEN}")

echo "==> Port-forwarding registry..."
kubectl port-forward svc/registry 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT

echo "==> Waiting for the registry to accept connections..."
ready=
for _ in $(seq 1 30); do
  if curl -s -o /dev/null "http://localhost:18080/v1/orgs" "${auth_header[@]}"; then
    ready=1; break
  fi
  sleep 1
done
[ -n "$ready" ] || { echo "ERROR: registry did not start serving on :18080 in time." >&2; exit 1; }

# create_org ORGID NAME ADMINUSER
create_org() {
  local org="$1" name="$2" admin="$3"
  echo "  org ${org} (admin=${admin})"
  curl -sf -X POST "http://localhost:18080/v1/orgs" \
    -H 'Content-Type: application/json' "${auth_header[@]}" \
    -d "{\"orgId\":\"${org}\",\"name\":\"${name}\",\"adminUser\":\"${admin}\"}" >/dev/null
}

# add_member ORGID USER ROLE
add_member() {
  local org="$1" user="$2" role="$3"
  echo "  member ${user} (${role}) -> ${org}"
  curl -sf -X POST "http://localhost:18080/v1/orgs/${org}/members" \
    -H 'Content-Type: application/json' "${auth_header[@]}" \
    -d "{\"user\":\"${user}\",\"role\":\"${role}\"}" >/dev/null
}

echo "==> Creating demo organizations..."
create_org acme   "Acme"   alice
add_member acme   viewer   member
create_org globex "Globex" bob
# carol is a plain member of BOTH orgs — the org-subdomains silent cross-subdomain
# SSO fixture (e2e case b). Added last so she never perturbs the single-org
# isolation matrix (which asserts on named users, not member counts).
add_member acme   carol    member
add_member globex carol    member

echo ""
echo "Done. Demo orgs seeded: acme (alice admin, viewer + carol members), globex (bob admin, carol member)."
