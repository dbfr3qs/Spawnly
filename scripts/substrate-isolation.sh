#!/usr/bin/env bash
# scripts/substrate-isolation.sh — the LIVE two-org isolation matrix
# (remote-substrate Phase 6, spec AC 1/4/5/6). Run AFTER scripts/substrate-e2e-setup.sh
# (org A active substrate + org B credentials). This is the shell half of the
# "test that matters": it drives REAL SVIDs, REAL node attestation, and the REAL
# Phase-1 Go verifier — the assertions the browser e2e can't reach — while the
# Playwright substrate-isolation.spec.ts covers the dashboard-facing cross-org
# refusals and invokes THIS for the identity layer.
#
# Matrix (org A = active substrate, org B = the "other org"):
#   (a) POSITIVE DIAGONAL — org A's org-scoped workload SVID verifies as org A
#       through the real registry verifier chain (AC 4 positive).
#   (b) ISOLATION INVARIANT — the SAME org-A SVID is REFUSED when bound to an org B
#       record (verifier called with -org B → fail closed). This is AC 5, the
#       hosted-SPIRE analog of the STS isolation invariant.
#   (c) NODE→ORG BINDING is cryptographic — org A's and org B's issued leaves are
#       CN-bound to their own org, so the server's x509pop agent_path_template can
#       only ever place each under /org/<its-own-org>/… (AC 1). An org-B credential
#       can never mint an org-A node path.
#   (d) FORGED / UNTRUSTED CA — a leaf off a CA the server doesn't trust FAILS node
#       attestation; the substrate never activates (AC 1 negative).
#
# Entry-API cross-org refusal (AC 6) and dispatch-channel own-org-only (AC 4/d) are
# exercised LIVE by the running substrate (every workload entry + every channel
# frame is org-scoped) and exhaustively proven headless in Go
# (internal/substrate.TestEntryAPIAuthzMatrix, TestHub_PushOwnOrgOnly) and JUnit
# (OrgAwareVerifier client-credentials + token-exchange). This script asserts the
# identity spine those rest on.
#
# Exit 0 iff EVERY cell holds. ⚠ LOCAL-SIM / e2e only.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ORG_A="${1:-${SUBSTRATE_ORG_A:-acme}}"
ORG_B="${2:-${SUBSTRATE_ORG_B:-globex}}"
SUBSTRATE_NS="${SUBSTRATE_NAMESPACE:-substrate-sim}"
SPIRE_NS="${SPIRE_NAMESPACE:-spire-system}"
TD="${SPIRE_TRUST_DOMAIN:-cluster.local}"
SERVER_POD="spire-server-0"
SERVER="kubectl -n ${SPIRE_NS} exec ${SERVER_POD} -c spire-server -- /opt/spire/bin/spire-server"
IDENTITY_SECRET_A="${SUBSTRATE_LEAF_SECRET:-substrate-identity}"
IDENTITY_SECRET_B="substrate-identity-${ORG_B}"
POD="substrate-isolation-proof"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "  PASS — $*"; }

[ "$ORG_A" != "$ORG_B" ] || fail "org A and org B must differ"

echo "== remote-substrate two-org isolation matrix: A='${ORG_A}' vs B='${ORG_B}' =="

# ── prerequisite: org A's substrate node has attested ─────────────────────────
# `|| true` so a missing Secret surfaces the friendly fail below, not a bare set -e abort.
FP_A="$(kubectl -n "$SUBSTRATE_NS" get secret "$IDENTITY_SECRET_A" -o jsonpath='{.data.svid\.crt}' 2>/dev/null \
  | base64 -d | openssl x509 -outform der 2>/dev/null | sha1sum | awk '{print $1}' || true)"
[ -n "$FP_A" ] || fail "org A identity Secret ${SUBSTRATE_NS}/${IDENTITY_SECRET_A} missing — run scripts/substrate-e2e-setup.sh"
NODE_A="spiffe://${TD}/spire/agent/x509pop/org/${ORG_A}/${FP_A}"
echo "==> waiting for org A node ${NODE_A}..."
found=0
for _ in $(seq 1 30); do
  $SERVER agent list 2>/dev/null | grep -qF "$NODE_A" && { found=1; break; }
  sleep 2
done
[ "$found" = "1" ] || fail "org A node did not attest — is the substrate sim up?"

# ── (c) cryptographic node→org binding: leaves are CN-bound to their own org ───
echo "==> (c) node→org binding is cryptographic (leaf CN == org)..."
cn_of() {
  kubectl -n "$SUBSTRATE_NS" get secret "$1" -o jsonpath='{.data.svid\.crt}' 2>/dev/null \
    | base64 -d | openssl x509 -noout -subject 2>/dev/null | grep -oE "CN ?= ?[^,/]+" | sed -E 's/CN ?= ?//' | tr -d '[:space:]'
}
CN_A="$(cn_of "$IDENTITY_SECRET_A" || true)"
CN_B="$(cn_of "$IDENTITY_SECRET_B" || true)"
[ "$CN_A" = "$ORG_A" ] || fail "org A leaf CN=${CN_A:-<none>} != ${ORG_A} (would misplace its node path)"
[ -n "$CN_B" ] || fail "org B credential Secret ${SUBSTRATE_NS}/${IDENTITY_SECRET_B} missing — run scripts/substrate-e2e-setup.sh"
[ "$CN_B" = "$ORG_B" ] || fail "org B leaf CN=${CN_B:-<none>} != ${ORG_B}"
# The server's agent_path_template is /spire/agent/x509pop/org/{{.Subject.CommonName}}/...
# so CN==org means org B's credential can only ever attest under /org/${ORG_B}/ — it
# is cryptographically incapable of producing an /org/${ORG_A}/ node path.
pass "org A leaf ⇒ /org/${ORG_A}/ only; org B leaf ⇒ /org/${ORG_B}/ only (CN-bound)"

# ── create a proof entry + pod under org A, fetch its org-scoped SVID ──────────
AGENT_PATH="spiffe://${TD}/org/${ORG_A}/agent/${SUBSTRATE_NS}/iso/proof/${POD}"
echo "==> minting an org-A workload SVID (entry ${AGENT_PATH})..."
if ! OUT="$($SERVER entry create -parentID "$NODE_A" -spiffeID "$AGENT_PATH" \
    -selector "k8s:ns:${SUBSTRATE_NS}" -selector "k8s:pod-name:${POD}" 2>&1)"; then
  printf '%s\n' "$OUT" | grep -qi "already exists" || fail "entry create failed: ${OUT}"
fi
kubectl -n "$SUBSTRATE_NS" delete pod "$POD" --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true
cat <<YAML | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${POD}, namespace: ${SUBSTRATE_NS} }
spec:
  restartPolicy: Never
  containers:
    - name: probe
      image: ghcr.io/spiffe/spire-agent:1.15.1
      command: ["/opt/spire/bin/spire-agent","api","watch","-socketPath","/spiffe-workload-api/spire-agent.sock"]
      volumeMounts: [{ name: spiffe-workload-api, mountPath: /spiffe-workload-api, readOnly: true }]
  volumes:
    - name: spiffe-workload-api
      csi: { driver: csi.substrate.spiffe.io, readOnly: true }
YAML

JWKS_PORT="${SUBSTRATE_JWKS_PORT:-18453}"
cleanup() {
  kill "${PF_PID:-}" 2>/dev/null || true
  kubectl -n "$SUBSTRATE_NS" delete pod "$POD" --ignore-not-found --grace-period=0 >/dev/null 2>&1 || true
}
trap cleanup EXIT
kubectl -n "$SUBSTRATE_NS" wait --for=condition=Ready "pod/${POD}" --timeout=90s >/dev/null

TOKEN="$(kubectl -n "$SUBSTRATE_NS" exec "$POD" -c probe -- \
  /opt/spire/bin/spire-agent api fetch jwt -audience registry \
  -socketPath /spiffe-workload-api/spire-agent.sock 2>/dev/null | sed -n '2p' | tr -d '[:space:]')"
[ -n "$TOKEN" ] || fail "could not fetch a JWT-SVID from the org-A proof pod"

kubectl -n "$SPIRE_NS" port-forward svc/spire-spiffe-oidc-discovery-provider "${JWKS_PORT}:443" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 20); do curl -sk "https://localhost:${JWKS_PORT}/keys" >/dev/null 2>&1 && break; sleep 1; done
JWKS="https://localhost:${JWKS_PORT}/keys"

# ── (a) positive diagonal: org-A SVID verifies as org A ───────────────────────
echo "==> (a) org-A SVID authenticates against an org-A record (diagonal)..."
(cd "$REPO_ROOT" && go run ./cmd/substrate-verify -token "$TOKEN" -jwks-url "$JWKS" -org "$ORG_A" -trust-domain "$TD") \
  || fail "the real verifier REJECTED org A's own SVID (diagonal must pass)"
pass "org-A SVID ⇒ org-A record verified"

# ── (b) isolation invariant: org-A SVID REFUSED against an org-B record ────────
echo "==> (b) org-A SVID is REFUSED against an org-B record (isolation invariant)..."
if b_out="$(cd "$REPO_ROOT" && go run ./cmd/substrate-verify -token "$TOKEN" -jwks-url "$JWKS" -org "$ORG_B" -trust-domain "$TD" 2>&1)"; then
  fail "org-A SVID was ACCEPTED for an org-B record — ISOLATION BREACH"
fi
# The rejection must be the ORG-MISMATCH, not an infra failure (a dead port-forward
# would also exit non-zero and falsely "pass" the invariant). The verifier prints
# "SVID org %q != expected org %q" on the isolation reject.
printf '%s' "$b_out" | grep -qiE "!= expected org" \
  || fail "org-B binding rejected but NOT for the org mismatch (infra failure?): ${b_out}"
pass "org-A SVID ⇒ org-B record REFUSED (fail closed, on org mismatch)"

# ── (d) forged / untrusted CA fails node attestation ──────────────────────────
echo "==> (d) a forged-CA leaf FAILS node attestation (substrate never activates)..."
"$REPO_ROOT/scripts/substrate-verify-forged.sh" "$ORG_A" >/dev/null \
  && pass "forged-CA leaf REJECTED (no node)" \
  || fail "forged-CA negative check failed"

echo ""
echo "PASS: two-org isolation matrix holds (AC 1/4/5 live; AC 6 Entry-API + channel"
echo "      own-org-only are Go/JUnit-proven and exercised by the running substrate)."
