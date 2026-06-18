#!/usr/bin/env bash
# Bring up the ENTIRE AWS-STS Spawnly environment on EKS and prove attestation:
#   terraform apply -> kubeconfig -> push images -> deploy -> smoke test
#
# Idempotent: safe to re-run (terraform converges, images rebuild via cache,
# deploy re-applies). Tear it all down with deploy/aws/down.sh.
#
# Env:
#   AWS_REGION   (default us-east-1) — must match the policy + terraform var
#   AI_PROVIDER / OPENAI_API_KEY (or ANTHROPIC_API_KEY / AI_API_KEY)
#       — only needed for the AI example agents; the worker smoke test does not
#         use the model, so up.sh runs fine without a key.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

export AWS_REGION="${AWS_REGION:-us-east-1}"
TF="terraform -chdir=deploy/aws/terraform"

echo "==> Preflight: tools + AWS credentials"
for t in terraform kubectl docker aws jq; do
  command -v "$t" >/dev/null || { echo "ERROR: missing required tool: $t" >&2; exit 1; }
done
aws sts get-caller-identity >/dev/null \
  || { echo "ERROR: AWS credentials not configured (aws sts get-caller-identity failed)" >&2; exit 1; }

echo "==> Terraform apply (EKS + IAM + ECR; first run ~15 min)"
$TF init -input=false >/dev/null
$TF apply -auto-approve

CLUSTER=$($TF output -raw cluster_name)
echo "==> Updating kubeconfig for cluster '$CLUSTER'"
aws eks update-kubeconfig --region "$AWS_REGION" --name "$CLUSTER" >/dev/null

echo "==> Building & pushing images to ECR"
./deploy/aws/push-images.sh

echo "==> Deploying platform (ATTESTOR=aws-sts, no SPIRE)"
./deploy/aws/deploy.sh

echo "==> Smoke test: spawn a worker and prove STS attestation"
./deploy/aws/smoke-test.sh

echo ""
echo "Environment is UP."
echo "  Dashboard:  kubectl port-forward svc/dashboard 8090:8080   (http://localhost:8090)"
echo "  Re-run the test:  ./deploy/aws/smoke-test.sh"
echo "  TEAR DOWN (stop AWS charges):  ./deploy/aws/down.sh"
