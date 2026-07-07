#!/usr/bin/env bash
# scripts/activate-anchor.sh — prove control of an org's STS trust anchor.
#
# Run this from a shell that holds the anchored AWS account's credentials (the
# same account/role named in the anchor's roleArnPattern). It:
#   1. presigns an STS GetCallerIdentity from those credentials (cmd/anchorctl,
#      which reuses the runtime sidecar's presign source), and
#   2. POSTs the presigned presentation to the org's activate endpoint through
#      the orchestrator with your delegated org-admin access token.
# The registry replays the presentation against STS and activates the anchor iff
# the attested account + assumed-role ARN match the registered anchor. Spawnly
# never sees your AWS credentials — only the presigned GetCallerIdentity.
#
# Usage:
#   ORCHESTRATOR_URL=https://spawnly.run \
#   ACCESS_TOKEN=<org-admin bearer token> \
#   ./scripts/activate-anchor.sh <orgId>
#
# For e2e against the in-cluster fake STS, additionally export
#   AWS_ENDPOINT_URL_STS=http://fake-sts:8080
# and dummy AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_REGION so the SDK can
# presign (the fake ignores the signature).
set -euo pipefail

ORG="${1:-}"
: "${ORG:?usage: activate-anchor.sh <orgId> (with ORCHESTRATOR_URL + ACCESS_TOKEN set)}"
: "${ORCHESTRATOR_URL:?set ORCHESTRATOR_URL to the orchestrator/edge base URL}"
: "${ACCESS_TOKEN:?set ACCESS_TOKEN to an org-admin delegated access token}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Presigning GetCallerIdentity from ambient AWS credentials..."
CRED="$(cd "$REPO_ROOT" && go run ./cmd/anchorctl)"

echo "==> Activating anchor for org '${ORG}' via ${ORCHESTRATOR_URL}..."
# The registry returns a generic 400 on any proof failure (oracle-free); -f makes
# curl surface a non-2xx as an error exit.
curl -sf -X POST "${ORCHESTRATOR_URL}/v1/orgs/${ORG}/anchor/activate" \
  -H "Authorization: Bearer ${ACCESS_TOKEN}" \
  -H 'Content-Type: application/json' \
  -d "{\"credential\":\"${CRED}\"}"
echo ""
echo "Anchor for '${ORG}' is now active."
