#!/usr/bin/env bash
# Build every platform/agent image from the repo's multi-stage Dockerfile and
# push it to ECR. Mirrors the Makefile's build logic (docker build --target
# <stage> -> agent-<stage>, with agent-sidecar as the one special case).
#
# Usage:
#   AWS_REGION=us-east-1 deploy/aws/push-images.sh [ECR_REGISTRY_HOST]
# ECR_REGISTRY_HOST defaults to the ecr root's `ecr_registry` output.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

REGION="${AWS_REGION:-us-east-1}"
TAG="${IMAGE_TAG:-latest}"
ECR="${1:-$(terraform -chdir=deploy/aws/ecr output -raw ecr_registry)}"

echo "==> Logging in to ECR ($ECR)"
aws ecr get-login-password --region "$REGION" \
  | docker login --username AWS --password-stdin "$ECR"

for svc in $(make -s print-SERVICES); do
  # agent-sidecar's stage and image are both "agent-sidecar"; everything else is
  # stage <svc> -> image agent-<svc>.
  if [ "$svc" = "agent-sidecar" ]; then img="agent-sidecar"; else img="agent-$svc"; fi
  echo "==> $img (stage: $svc)"
  docker build --load --target "$svc" -t "$ECR/$img:$TAG" .
  docker push "$ECR/$img:$TAG"
done

echo "==> Done. Pushed all images to $ECR"
