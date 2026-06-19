#!/usr/bin/env bash
# Tear down the ENTIRE AWS environment so it stops costing money.
# Deletes the in-cluster resources (best-effort, to avoid orphaned cloud
# resources) then destroys all Terraform-managed infra (EKS, VPC, IAM, ECR).
#
# Not set -e: the kubectl cleanup is best-effort; terraform destroy is the part
# that must run regardless.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)"
export AWS_REGION="${AWS_REGION:-us-east-1}"

echo "==> Deleting in-cluster resources (best-effort)"
if kubectl cluster-info >/dev/null 2>&1; then
  kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/aws 2>/dev/null \
    | kubectl delete -f - --ignore-not-found 2>/dev/null || true
  kubectl delete sa spawnly-agent --ignore-not-found 2>/dev/null || true
  kubectl delete secret control-plane-auth ai-provider --ignore-not-found 2>/dev/null || true
else
  echo "   (kubectl can't reach a cluster — skipping; terraform destroy will remove it)"
fi

echo "==> terraform destroy (EKS + VPC + IAM + ECR + Pod Identity addon/association)"
terraform -chdir=deploy/aws/terraform destroy -auto-approve

echo ""
echo "Environment is DOWN. Verify nothing lingers:"
echo "  aws eks list-clusters --region $AWS_REGION"
echo "  aws ecr describe-repositories --region $AWS_REGION 2>/dev/null | jq '.repositories[].repositoryName'"
echo ""
echo "Note: outbound web identity federation is an account-level capability left"
echo "      ENABLED (harmless). To revert it explicitly:"
echo "      aws iam disable-outbound-web-identity-federation"
