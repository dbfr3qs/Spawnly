#!/usr/bin/env bash
# Tear down the cluster so it stops costing money. Destroys the CLUSTER root only
# (EKS, VPC, IAM, Pod Identity). The ECR repositories live in their own root
# (deploy/aws/ecr) and are intentionally LEFT INTACT so images survive — push
# once, reuse across down/up. To delete the images too: ./deploy/aws/destroy-ecr.sh
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

echo "==> terraform destroy (cluster root: EKS + VPC + IAM + Pod Identity)"
terraform -chdir=deploy/aws/terraform destroy -auto-approve

echo ""
echo "Cluster is DOWN. ECR repositories (deploy/aws/ecr) were KEPT — images persist"
echo "for the next 'up.sh' (no re-push needed). Verify:"
echo "  aws eks list-clusters --region $AWS_REGION"
echo "  aws ecr describe-repositories --region $AWS_REGION 2>/dev/null | jq '.repositories[].repositoryName'"
echo ""
echo "To also delete the images + repos:  ./deploy/aws/destroy-ecr.sh"
echo "Note: outbound web identity federation is left ENABLED (account-level, harmless)."
echo "      Revert with: aws iam disable-outbound-web-identity-federation"
