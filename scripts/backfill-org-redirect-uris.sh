#!/usr/bin/env bash
# scripts/backfill-org-redirect-uris.sh — (re)register every organization's
# satellite redirect URI on the dashboard Keycloak client.
#
# For each org (its alias is the subdomain slug) it registers, on the `dashboard`
# client, the redirect URI  https://<slug>.<apex>/callback  — alongside the apex's
# own callback. It reads the org set from Keycloak's Organizations (the source of
# truth), so it is COMPLETE and idempotent. (Only redirect URIs: the dashboard
# logs out to the APEX post-logout URI regardless of the satellite, so the per-org
# post-logout URIs org-subdomains registers at onboarding are not on the login or
# logout path and don't need repairing here.)
#
# WHY: org-subdomains registers each org's satellite callback at ONBOARDING via
# the admin API, but the dashboard client's redirect URIs are Terraform-managed,
# so a bare `terraform apply` (every realm re-apply / AWS deploy) reconciles the
# runtime-added per-tenant URIs back to the declared apex-only list — after which
# a satellite login (member OR the master-admin view-as-org) fails with Keycloak
# "Invalid parameter: redirect_uri". Run this AFTER the realm apply (deploy.sh /
# scripts/local-tls.sh do) to re-establish them; run it standalone to repair.
#
# Usage: scripts/backfill-org-redirect-uris.sh <apex-domain> [apex-origin]
#   <apex-domain>  the satellite apex, e.g. spawnly.run / localtest.me
#   [apex-origin]  the dashboard's own apex origin whose /callback to keep
#                  (default https://<apex-domain>; e.g. https://spawnly.run)
#
# Requires: a running keycloak deployment + the keycloak-admin secret, kubectl.
set -euo pipefail

APEX="${1:-${ORG_APEX_DOMAIN:-}}"
if [ -z "$APEX" ]; then
  echo "usage: $0 <apex-domain> [apex-origin]   (or set ORG_APEX_DOMAIN)" >&2
  exit 1
fi
APEX_ORIGIN="${2:-https://$APEX}"
REALM="${KEYCLOAK_REALM:-spawnly}"
ADMIN_PW="$(kubectl get secret keycloak-admin -o jsonpath='{.data.password}' | base64 -d)"
KC=/opt/keycloak/bin/kcadm.sh
KC_LOGIN="$KC config credentials --server http://localhost:8080 --realm master --user admin --password '$ADMIN_PW' >/dev/null 2>&1"

# Org aliases (the subdomain slugs) straight from Keycloak Organizations.
ALIASES=$(kubectl exec deploy/keycloak -- bash -c \
  "$KC_LOGIN; $KC get organizations -r $REALM --fields alias --format csv --noquotes" \
  | tr -d '\r' | sed '/^[[:space:]]*$/d')

# Build the COMPLETE redirect-URI list: the apex, then every org satellite.
redir="\"$APEX_ORIGIN/callback\""
count=0
while IFS= read -r a; do
  [ -n "$a" ] || continue
  redir="$redir,\"https://$a.$APEX/callback\""
  count=$((count + 1))
done <<<"$ALIASES"

kubectl exec deploy/keycloak -- bash -c "
  $KC_LOGIN
  CID=\$($KC get clients -r $REALM -q clientId=dashboard --fields id --format csv --noquotes | tail -1)
  [ -n \"\$CID\" ] || { echo 'dashboard client not found' >&2; exit 1; }
  $KC update clients/\$CID -r $REALM -s 'redirectUris=[$redir]'
"
echo "Done. dashboard client redirect URIs: apex ($APEX_ORIGIN) + ${count} org satellite(s) under ${APEX}."
