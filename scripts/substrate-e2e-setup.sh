#!/usr/bin/env bash
# scripts/substrate-e2e-setup.sh — the FIRST-CLASS remote-substrate e2e harness
# (Phase 6). Stands up a LIVE two-org substrate simulation on a bootstrapped kind
# cluster so the substrate Playwright specs (substrate.spec, substrate-dispatch,
# substrate-lifecycle, substrate-isolation) run end-to-end instead of skipping:
#
#   • org A (default: acme)  — a FULLY LIVE substrate: dev CA + x509pop on the
#     hosted server, the deploy/substrate/ overlay (SPIRE agent + CSI + operator +
#     substrate-agent), AND the Entry API delegated-admin socket wired so a connect
#     activates the dispatch channel and member spawns land on the substrate.
#   • org B (default: globex) — CREDENTIALS ONLY: a valid org-bound x509 leaf in
#     Secret substrate-sim/substrate-identity-<orgB>, the raw material the isolation
#     matrix uses to prove org A's active substrate can never act for org B (and a
#     legitimately-signed org-B node still can't reach org A's records/entries/work).
#
# Mirrors the make e2e-anchor-setup / deploy-fake-sts.sh stance: e2e-only, NOT part
# of the kustomize base or bootstrap, idempotent, and reversible with
# `make substrate-down`. Run after `make bootstrap` (+ `make e2e-anchor-setup` for
# the demo orgs). ⚠ LOCAL-SIM / e2e ONLY — see the header of each sub-script.
#
# Usage:
#   ./scripts/substrate-e2e-setup.sh                 # A=acme, B=globex
#   SUBSTRATE_ORG_A=acme SUBSTRATE_ORG_B=globex ./scripts/substrate-e2e-setup.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORG_A="${SUBSTRATE_ORG_A:-${1:-acme}}"
ORG_B="${SUBSTRATE_ORG_B:-${2:-globex}}"
SUBSTRATE_NS="${SUBSTRATE_NAMESPACE:-substrate-sim}"

if [ "$ORG_A" = "$ORG_B" ]; then
  echo "ERROR: org A and org B must differ (got '${ORG_A}')" >&2
  exit 1
fi

echo "======================================================================"
echo " remote-substrate e2e harness: org A='${ORG_A}' (active) · org B='${ORG_B}' (creds)"
echo "======================================================================"

# 1. Live Entry API broker (shared delegated-admin socket) so connect can provision
#    the substrate-agent bootstrap entry + workload entries and the channel activates.
echo ""
echo "### Wiring the Entry API delegated-admin socket (live dispatch/channel)..."
"$REPO_ROOT/scripts/substrate-wire-entry-api.sh"

# 2. Org A: the fully-live substrate (dev CA + x509pop + overlay + identity).
echo ""
echo "### Standing up org A ('${ORG_A}') live substrate..."
"$REPO_ROOT/scripts/substrate-setup.sh" "$ORG_A"

# 3. Org B: credentials only — a valid org-bound leaf for the negative/isolation
#    tests. No second overlay: one substrate per org in the sim, and org B exists
#    here purely as the "other org" whose identity must be refused everywhere.
echo ""
echo "### Issuing org B ('${ORG_B}') credentials (isolation-matrix negatives)..."
SUBSTRATE_LEAF_SECRET="substrate-identity-${ORG_B}" \
  "$REPO_ROOT/scripts/substrate-dev-cert.sh" "$ORG_B" "$SUBSTRATE_NS"

echo ""
echo "======================================================================"
echo " Harness ready."
echo "   • org A ('${ORG_A}') substrate is live — connect + spawn onto it."
echo "   • org B ('${ORG_B}') credentials in Secret ${SUBSTRATE_NS}/substrate-identity-${ORG_B}."
echo ""
echo " Prove it:"
echo "   ./scripts/substrate-verify.sh ${ORG_A}                    # identity loop (AC 1/4)"
echo "   ./scripts/substrate-isolation.sh ${ORG_A} ${ORG_B}          # two-org matrix (AC 4/5/6)"
echo "   cd e2e && npx playwright test substrate                   # the substrate specs"
echo "======================================================================"
