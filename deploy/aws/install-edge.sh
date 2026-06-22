#!/usr/bin/env bash
# Install the public-edge controllers via Helm: the AWS Load Balancer Controller
# (provisions the ALB from the Ingress) and external-dns (writes the apex/auth
# Route53 records → ALB). Their IAM is provisioned by Terraform (cluster root,
# edge.tf) via EKS Pod Identity, so the ServiceAccounts need no IRSA annotation.
# Run after the cluster is up and kubectl points at it.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

REGION="${AWS_REGION:-us-east-1}"
CLUSTER="${CLUSTER:-spawnly}"
DOMAIN="${DOMAIN:-spawnly.run}"
VPC_ID="$(terraform -chdir=deploy/aws/terraform output -raw vpc_id 2>/dev/null || true)"

helm repo add eks https://aws.github.io/eks-charts >/dev/null 2>&1 || true
helm repo add external-dns https://kubernetes-sigs.github.io/external-dns >/dev/null 2>&1 || true
helm repo update eks external-dns >/dev/null

echo "==> AWS Load Balancer Controller"
helm upgrade --install aws-load-balancer-controller eks/aws-load-balancer-controller \
  -n kube-system \
  --set clusterName="$CLUSTER" \
  --set region="$REGION" \
  ${VPC_ID:+--set vpcId="$VPC_ID"} \
  --set serviceAccount.create=true \
  --set serviceAccount.name=aws-load-balancer-controller \
  --wait

echo "==> external-dns (zone: $DOMAIN)"
helm upgrade --install external-dns external-dns/external-dns \
  -n kube-system \
  --set provider=aws \
  --set policy=sync \
  --set registry=txt \
  --set txtOwnerId="$CLUSTER" \
  --set "domainFilters[0]=$DOMAIN" \
  --set "sources[0]=ingress" \
  --set serviceAccount.create=true \
  --set serviceAccount.name=external-dns \
  --wait

echo "Edge controllers installed."
