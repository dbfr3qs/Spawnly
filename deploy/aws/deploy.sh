#!/usr/bin/env bash
# Deploy the Spawnly platform onto EKS with the AWS-STS attestor (no SPIRE).
# Run AFTER `terraform apply` (deploy/aws/terraform) and after pushing images
# (deploy/aws/push-images.sh). Assumes kubectl is pointed at the EKS cluster.
#
# Required env:
#   AI_API_KEY (or ANTHROPIC_API_KEY / OPENAI_API_KEY)  — for agents to run
# Optional env:
#   AWS_REGION (default us-east-1), AI_PROVIDER, AI_MODEL, IMAGE_TAG (default latest)
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

REGION="${AWS_REGION:-us-east-1}"
TAG="${IMAGE_TAG:-latest}"
TF="terraform -chdir=deploy/aws/terraform"
ECR="${ECR_REGISTRY:-$($TF output -raw ecr_registry)}"
AGENT_ROLE_ARN="${AGENT_ROLE_ARN:-$($TF output -raw agent_role_arn)}"

# ── 1. Control-plane shared secret (reuse if it already exists) ────────────────
echo "==> control-plane-auth secret"
CP_TOKEN=$(kubectl get secret control-plane-auth -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
[ -z "$CP_TOKEN" ] && CP_TOKEN=$(openssl rand -hex 32)
kubectl create secret generic control-plane-auth \
  --from-literal=auth="shared-secret" --from-literal=token="$CP_TOKEN" \
  --dry-run=client -o yaml | kubectl apply -f -

# ── 2. AI provider secret (from env) ──────────────────────────────────────────
echo "==> ai-provider secret"
_AI_PROVIDER="${AI_PROVIDER:-anthropic}"
if [ "$_AI_PROVIDER" = "openai" ]; then
  _AI_API_KEY="${AI_API_KEY:-${OPENAI_API_KEY:-}}"; _AI_MODEL="${AI_MODEL:-openai/gpt-4o}"
else
  _AI_API_KEY="${AI_API_KEY:-${ANTHROPIC_API_KEY:-}}"; _AI_MODEL="${AI_MODEL:-anthropic/claude-sonnet-4-6}"
fi
[ -z "$_AI_API_KEY" ] && echo "  WARNING: no AI_API_KEY set — agents won't be able to call the model."
kubectl create secret generic ai-provider \
  --from-literal=provider="$_AI_PROVIDER" --from-literal=api-key="$_AI_API_KEY" --from-literal=model="$_AI_MODEL" \
  --dry-run=client -o yaml | kubectl apply -f -

# ── 3. Bind the agent ServiceAccount to the IAM role ──────────────────────────
echo "==> agent ServiceAccount role annotation"
sed "s#REPLACE_WITH_terraform_output_agent_role_arn#${AGENT_ROLE_ARN}#" \
  deploy/aws/serviceaccount.yaml | kubectl apply -f -

# ── 4. Apply the AWS overlay (ATTESTOR=aws-sts, no SPIRE) ──────────────────────
echo "==> applying platform manifests"
# `kubectl apply -k` doesn't accept --load-restrictor; build with `kubectl
# kustomize` (which does) and pipe. The flag is needed because the overlay
# references ../manifests and ../crds outside deploy/aws.
kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/aws | kubectl apply -f -

# ── 5. Point images at ECR (dynamic registry host) ────────────────────────────
echo "==> repointing images at ECR ($ECR)"
for pair in \
  "registry agent-registry" "identity-server agent-identity-server" \
  "orchestrator agent-orchestrator" "dashboard agent-dashboard" \
  "agent-operator agent-operator" "sample-api agent-sample-api" \
  "sample-api-a agent-sample-api" "sample-api-b agent-sample-api" \
  "sample-api-global agent-sample-api"; do
  set -- $pair
  kubectl set image "deployment/$1" "*=$ECR/$2:$TAG"
  # Base manifests use imagePullPolicy:Never (a kind side-load convention); on
  # EKS the node must pull from ECR. Use Always (not IfNotPresent) because we
  # iterate on the mutable :latest tag — IfNotPresent would serve a stale image
  # the node already cached.
  kubectl patch "deployment/$1" --type=json \
    -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Always"}]'
done
# The agent-sidecar image is injected into agent pods by the operator.
kubectl set env deployment/agent-operator "SIDECAR_IMAGE=$ECR/agent-sidecar:$TAG"

# ── 6. Wait for rollouts ──────────────────────────────────────────────────────
echo "==> waiting for services"
kubectl wait --for=condition=ready pod -l app=spicedb --timeout=180s || true
for d in registry identity-server sample-api sample-api-a sample-api-b \
         sample-api-global agent-operator orchestrator dashboard; do
  kubectl rollout status "deployment/$d" --timeout=180s
done

# ── 7. Seed templates (ECR-qualified agent images) ────────────────────────────
echo "==> seeding templates"
ECR_REGISTRY="$ECR" IMAGE_TAG="$TAG" deploy/aws/seed-aws.sh

echo ""
echo "Deploy complete. Port-forward the dashboard:  kubectl port-forward svc/dashboard 8090:8080"
