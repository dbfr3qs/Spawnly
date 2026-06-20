#!/usr/bin/env bash
# Bring up the ENTIRE Spawnly environment on EKS (hardened aws-stsweb attestor)
# and prove attestation:
#   terraform apply -> kubeconfig -> access-entry self-heal -> enable outbound
#   federation -> push images -> deploy -> smoke test
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
for t in terraform kubectl docker aws jq helm; do
  command -v "$t" >/dev/null || { echo "ERROR: missing required tool: $t" >&2; exit 1; }
done
CALLER_PREFLIGHT=$(aws sts get-caller-identity --query Arn --output text 2>/dev/null) \
  || { echo "ERROR: AWS credentials not configured/expired (aws sts get-caller-identity failed)." >&2
       echo "       If on SSO: aws sso login --profile \"\${AWS_PROFILE:-}\"" >&2; exit 1; }
echo "    identity: $CALLER_PREFLIGHT"
case "$CALLER_PREFLIGHT" in
  *:assumed-role/AWSReservedSSO*)
    echo "    WARNING: this is an AWS SSO session — it can expire mid-run (~30 min total)." >&2
    echo "             If a step fails with 'session expired', run: aws sso login --profile \"\${AWS_PROFILE:-}\"" >&2
    echo "             A static-key IAM user avoids this entirely." >&2 ;;
esac

# Public exposure is opt-in: it's ON only when the persistent DNS root
# (deploy/aws/dns) is applied. This single flag gates the edge IAM (Terraform),
# the controller install, and the ingress — so a plain deploy provisions no edge
# and never tries to resolve the (absent) hosted zone.
EDGE=false
terraform -chdir=deploy/aws/dns output -raw acm_certificate_arn >/dev/null 2>&1 && EDGE=true
export TF_VAR_enable_public_edge="$EDGE"
echo "==> Public edge: $([ "$EDGE" = true ] && echo 'ON (DNS root applied)' || echo 'off (deploy/aws/dns not applied)')"

# ECR lives in its own root/state so images survive `down.sh` (push once, reuse).
# Idempotent: a no-op when the repos already exist from a previous cycle.
echo "==> Terraform apply (ECR repositories)"
terraform -chdir=deploy/aws/ecr init -input=false >/dev/null
terraform -chdir=deploy/aws/ecr apply -auto-approve

echo "==> Terraform apply (EKS + IAM; first run ~15 min)"
$TF init -input=false >/dev/null
$TF apply -auto-approve

CLUSTER=$($TF output -raw cluster_name)
echo "==> Updating kubeconfig for cluster '$CLUSTER'"
# Pin the active profile into the kubeconfig exec so kubectl always resolves the
# SAME identity Terraform used — otherwise the token plugin falls back to the
# shell default (e.g. an SSO role), which won't have the access entry we grant.
aws eks update-kubeconfig --region "$AWS_REGION" --name "$CLUSTER" \
  ${AWS_PROFILE:+--profile "$AWS_PROFILE"} >/dev/null

# Access-entry self-heal: enable_cluster_creator_admin_permissions stores a
# mismatched ARN for AWS SSO roles, which 401s kubectl. Grant the *current*
# caller cluster-admin explicitly, converting an SSO assumed-role session ARN to
# its underlying IAM role ARN (which carries the aws-reserved/sso path).
echo "==> Ensuring kubectl access (EKS access entry for the caller)"
CALLER_ARN=$(aws sts get-caller-identity --query Arn --output text)
case "$CALLER_ARN" in
  *:assumed-role/*)
    ROLE_NAME=$(printf '%s' "$CALLER_ARN" | sed -E 's#.*:assumed-role/([^/]+)/.*#\1#')
    PRINCIPAL_ARN=$(aws iam list-roles --query "Roles[?RoleName=='${ROLE_NAME}'].Arn" --output text) ;;
  *) PRINCIPAL_ARN="$CALLER_ARN" ;;
esac
if [ -n "$PRINCIPAL_ARN" ] && [ "$PRINCIPAL_ARN" != "None" ]; then
  ADMIN_POLICY=arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy
  aws eks create-access-entry --cluster-name "$CLUSTER" --region "$AWS_REGION" \
    --principal-arn "$PRINCIPAL_ARN" --type STANDARD >/dev/null 2>&1 || true
  # The associate flag name differs across CLI versions (--access-policy-arn vs
  # --policy-arn); try both so the admin policy actually attaches.
  aws eks associate-access-policy --cluster-name "$CLUSTER" --region "$AWS_REGION" \
    --principal-arn "$PRINCIPAL_ARN" --access-scope type=cluster \
    --access-policy-arn "$ADMIN_POLICY" >/dev/null 2>&1 \
  || aws eks associate-access-policy --cluster-name "$CLUSTER" --region "$AWS_REGION" \
    --principal-arn "$PRINCIPAL_ARN" --access-scope type=cluster \
    --policy-arn "$ADMIN_POLICY" >/dev/null 2>&1 || true
fi
kubectl get nodes >/dev/null 2>&1 || { echo "ERROR: kubectl can't reach the cluster (access entry?)" >&2; exit 1; }

# Outbound web identity federation (account-level, one-time, idempotent). The
# issuer it returns is the JWKS the verifiers validate web-identity tokens against.
echo "==> Ensuring outbound web identity federation is enabled"
aws iam enable-outbound-web-identity-federation >/dev/null 2>&1 || true
export STSWEB_ISSUER=$(aws iam get-outbound-web-identity-federation-info --query IssuerIdentifier --output text)
echo "    STS issuer: $STSWEB_ISSUER"

# Public edge controllers — gated on the same flag computed above.
if [ "$EDGE" = true ]; then
  echo "==> Installing public-edge controllers (ALB controller + external-dns)"
  ./deploy/aws/install-edge.sh
fi

echo "==> Building & pushing images to ECR"
./deploy/aws/push-images.sh

echo "==> Deploying platform (ATTESTOR=aws-stsweb, no SPIRE)"
./deploy/aws/deploy.sh

echo "==> Smoke test: spawn a worker and prove STS attestation"
./deploy/aws/smoke-test.sh

echo ""
echo "Environment is UP."
echo "  Dashboard:  kubectl port-forward svc/dashboard 8090:8080   (http://localhost:8090)"
echo "  Re-run the test:  ./deploy/aws/smoke-test.sh"
echo "  TEAR DOWN (stop AWS charges):  ./deploy/aws/down.sh"
