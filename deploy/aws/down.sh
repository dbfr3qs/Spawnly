#!/usr/bin/env bash
# Tear down the cluster so it stops costing money.
#
#   ./deploy/aws/down.sh          Destroy the CLUSTER root only (EKS, VPC, IAM,
#                                 Pod Identity). The persistent roots — ECR images
#                                 (deploy/aws/ecr) and DNS+TLS (deploy/aws/dns) —
#                                 are LEFT INTACT so images persist and the
#                                 registrar NS delegation stays valid.
#   ./deploy/aws/down.sh --all    ALSO destroy ECR and DNS+TLS. Full teardown:
#                                 stops every cost, but deletes the image repos
#                                 AND breaks the registrar nameserver delegation
#                                 (you'd re-delegate on the next setup).
#
# Not set -e: the kubectl cleanup is best-effort; terraform destroy must run.
set -uo pipefail

cd "$(git rev-parse --show-toplevel)"
export AWS_REGION="${AWS_REGION:-us-east-1}"

ALL=false
[ "${1:-}" = "--all" ] && ALL=true

echo "==> Deleting in-cluster resources (best-effort)"
if kubectl cluster-info >/dev/null 2>&1; then
  # Delete Ingresses first so the AWS LB Controller tears down the ALB and
  # external-dns removes its Route53 records — otherwise a hard cluster destroy
  # orphans the ALB + leaves DNS records pointing at nothing.
  kubectl delete ingress --all --all-namespaces --ignore-not-found 2>/dev/null || true
  sleep 10
  kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/aws 2>/dev/null \
    | kubectl delete -f - --ignore-not-found 2>/dev/null || true
  kubectl delete sa spawnly-agent --ignore-not-found 2>/dev/null || true
  kubectl delete secret control-plane-auth ai-provider --ignore-not-found 2>/dev/null || true
else
  echo "   (kubectl can't reach a cluster — skipping; terraform destroy will remove it)"
fi

echo "==> terraform destroy (cluster root: EKS + VPC + IAM + Pod Identity)"
terraform -chdir=deploy/aws/terraform destroy -auto-approve

if [ "$ALL" = true ]; then
  # init first: these persistent roots aren't applied by up.sh, and .terraform/
  # is gitignored, so a fresh checkout has no provider plugins to destroy with.
  echo "==> --all: terraform destroy (ECR images)"
  terraform -chdir=deploy/aws/ecr init -input=false >/dev/null
  terraform -chdir=deploy/aws/ecr destroy -auto-approve
  echo "==> --all: terraform destroy (DNS + TLS)  [breaks registrar NS delegation]"
  terraform -chdir=deploy/aws/dns init -input=false >/dev/null
  terraform -chdir=deploy/aws/dns destroy -auto-approve
  echo ""
  echo "FULL teardown complete — nothing should be billing. Note: the domain's"
  echo "registrar still points at the now-deleted zone's nameservers; re-delegate"
  echo "on the next setup (deploy/aws/dns output name_servers)."
  exit 0
fi

echo ""
echo "Cluster is DOWN. Persistent roots KEPT: ECR images (deploy/aws/ecr) and"
echo "DNS+TLS (deploy/aws/dns) — so the next 'up.sh' skips the image re-push and"
echo "DNS keeps resolving. Verify:"
echo "  aws eks list-clusters --region $AWS_REGION"
echo "  aws ecr describe-repositories --region $AWS_REGION 2>/dev/null | jq '.repositories[].repositoryName'"
echo ""
echo "Full teardown (also ECR + DNS):  ./deploy/aws/down.sh --all"
echo "Note: outbound web identity federation is left ENABLED (account-level, harmless)."
echo "      Revert with: aws iam disable-outbound-web-identity-federation"
