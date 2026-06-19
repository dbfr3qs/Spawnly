#!/usr/bin/env bash
# Deliberately delete the ECR repositories AND their images (the ecr root).
# down.sh does NOT do this — images persist across cluster down/up by default,
# so you only need this for a full cleanup.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)"
export AWS_REGION="${AWS_REGION:-us-east-1}"

echo "==> Destroying ECR repositories + images (deploy/aws/ecr)"
terraform -chdir=deploy/aws/ecr destroy -auto-approve

echo "ECR deleted. A later up.sh recreates the repos empty; push-images.sh repopulates."
