#!/usr/bin/env bash
# Seed agent templates into the EKS registry, ECR-qualifying each template's
# runtimeSpec.image so the operator can pull agent images from ECR. The
# kind/SPIRE equivalent is scripts/seed.sh (which seeds the bare image names).
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

TAG="${IMAGE_TAG:-latest}"
TF="terraform -chdir=deploy/aws/terraform"
ECR="${ECR_REGISTRY:-$($TF output -raw ecr_registry)}"

templates=(agents/*/template.json)
found=()
for f in "${templates[@]}"; do [ -f "$f" ] && found+=("$f"); done
[ "${#found[@]}" -eq 0 ] && { echo "ERROR: no template.json files found." >&2; exit 1; }

CP_TOKEN=$(kubectl get secret control-plane-auth -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
auth_header=()
[ -n "$CP_TOKEN" ] && auth_header=(-H "Authorization: Bearer ${CP_TOKEN}")

echo "==> port-forwarding registry"
kubectl port-forward svc/registry 18080:8080 >/dev/null 2>&1 &
PF_PID=$!
trap 'kill $PF_PID 2>/dev/null || true' EXIT

echo "==> waiting for registry"
ready=
for _ in $(seq 1 30); do
  curl -s -o /dev/null "http://localhost:18080/v1/templates" && { ready=1; break; }
  sleep 1
done
[ -n "$ready" ] || { echo "ERROR: registry not serving on :18080 in time." >&2; exit 1; }

echo "==> seeding templates (images -> $ECR)"
for f in "${found[@]}"; do
  agent_type=$(jq -r .agentType "$f")
  # Prepend the ECR host to runtimeSpec.image (e.g. agent-go-worker:latest ->
  # <ECR>/agent-go-worker:latest). The image tag in the template is preserved.
  jq --arg ecr "$ECR" '.runtimeSpec.image = ($ecr + "/" + .runtimeSpec.image)' "$f" \
    | curl -sf -X POST http://localhost:18080/v1/templates \
        -H 'Content-Type: application/json' "${auth_header[@]}" --data-binary @- >/dev/null
  echo "  ${agent_type}"
done

echo "Done seeding."
