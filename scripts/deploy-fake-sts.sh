#!/usr/bin/env bash
# scripts/deploy-fake-sts.sh — deploy the E2E-ONLY fake STS stub and point the
# registry's STS trust at it, for the anchor e2e (make e2e).
#
# ⚠ E2E ONLY. This builds+loads the fake-sts image (a trust-injection stub with
# NO SigV4 validation, see cmd/fake-sts/main.go), applies its standalone manifest
# (NOT in the kustomize base), and sets STS_TRUSTED_ENDPOINT on the registry so
# the anchor proof-of-control replay resolves against the fake. A real deploy
# runs NONE of this: fake-sts is absent from the base and the registry never
# carries STS_TRUSTED_ENDPOINT unless this script sets it. Do not run against a
# real cluster.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER="${KIND_CLUSTER:-agent-platform}"
FAKE_STS_ENDPOINT="http://fake-sts:8080"

echo "==> Building + loading fake-sts image (e2e-only)..."
docker build --target fake-sts -t agent-fake-sts:latest "$REPO_ROOT"
kind load docker-image agent-fake-sts:latest --name "$CLUSTER"

echo "==> Applying fake-sts manifest..."
kubectl apply -f "$REPO_ROOT/deploy/manifests/fake-sts.yaml"
kubectl rollout status deployment/fake-sts --timeout=120s

# Point the registry at the fake as its SOLE trusted STS endpoint. kubectl set env
# only rolls the deployment when the value actually changes, so re-running this is
# a no-op after the first time.
echo "==> Pointing registry STS_TRUSTED_ENDPOINT at ${FAKE_STS_ENDPOINT}..."
current="$(kubectl get deploy/registry -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="STS_TRUSTED_ENDPOINT")].value}' 2>/dev/null || true)"
if [ "$current" != "$FAKE_STS_ENDPOINT" ]; then
  kubectl set env deployment/registry "STS_TRUSTED_ENDPOINT=${FAKE_STS_ENDPOINT}"
  kubectl rollout status deployment/registry --timeout=120s
  # The registry's in-memory store was wiped by the restart — reseed templates +
  # demo orgs so the anchor e2e has acme/globex to work with.
  echo "==> Reseeding templates + demo orgs after registry restart..."
  "$REPO_ROOT/scripts/seed.sh"
  "$REPO_ROOT/scripts/reseed-orgs.sh"
else
  echo "    already set; skipping registry roll + reseed."
fi

echo "Done. fake-sts is up and the registry trusts it (e2e only)."
