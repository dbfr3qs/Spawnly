#!/usr/bin/env bash
# Deploy the Spawnly platform onto EKS with the hardened aws-stsweb attestor
# (EKS Pod Identity + STS GetWebIdentityToken; no SPIRE). Run AFTER
# `terraform apply` (deploy/aws/terraform) and after pushing images
# (deploy/aws/push-images.sh). Assumes kubectl is pointed at the EKS cluster,
# and that outbound web identity federation is enabled on the account.
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
ECR="${ECR_REGISTRY:-$(terraform -chdir=deploy/aws/ecr output -raw ecr_registry)}"
AGENT_SA="$($TF output -raw agent_service_account)"
CLUSTER_ARN="$($TF output -raw cluster_arn)"
STSWEB_AUDIENCE="${STSWEB_AUDIENCE:-spawnly}"
# The account STS issuer (from enabling outbound web identity federation) — the
# JWKS the verifiers validate web-identity tokens against.
STSWEB_ISSUER="${STSWEB_ISSUER:-$(aws iam get-outbound-web-identity-federation-info --query IssuerIdentifier --output text 2>/dev/null || true)}"
if [ -z "$STSWEB_ISSUER" ] || [ "$STSWEB_ISSUER" = "None" ]; then
  echo "ERROR: outbound web identity federation is not enabled on this account." >&2
  echo "       Run: aws iam enable-outbound-web-identity-federation" >&2
  exit 1
fi

# Guard: deployments pull from ECR. Fail early with a clear message if the images
# aren't there (e.g. running deploy.sh standalone on a freshly recreated, empty
# ECR — terraform destroy force-deletes the repos). up.sh chains push-images first.
if ! aws ecr describe-images --repository-name agent-registry \
     --image-ids imageTag="$TAG" --region "$REGION" >/dev/null 2>&1; then
  echo "ERROR: agent-registry:$TAG not found in ECR — push images first:" >&2
  echo "       ./deploy/aws/push-images.sh    (or just use ./deploy/aws/up.sh)" >&2
  exit 1
fi

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

# ── 3. Agent ServiceAccount (Pod Identity binds it to the role; no annotation) ──
echo "==> agent ServiceAccount $AGENT_SA"
kubectl create serviceaccount "$AGENT_SA" --dry-run=client -o yaml | kubectl apply -f -

# ── 4. Apply the AWS overlay (ATTESTOR=aws-stsweb, no SPIRE) ───────────────────
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

# ── 5b. Configure the aws-stsweb attestor (dynamic, account-specific values) ───
echo "==> configuring aws-stsweb env (issuer=$STSWEB_ISSUER)"
# Operator injects region + audience onto agent sidecars.
kubectl set env deployment/agent-operator AWS_REGION="$REGION" STSWEB_AUDIENCE="$STSWEB_AUDIENCE"
# The verifiers validate the web-identity JWT and assert the attested context.
for d in registry identity-server; do
  kubectl set env "deployment/$d" \
    STSWEB_ISSUER="$STSWEB_ISSUER" \
    STSWEB_AUDIENCE="$STSWEB_AUDIENCE" \
    STSWEB_NAMESPACE=default \
    STSWEB_SERVICE_ACCOUNT="$AGENT_SA" \
    STSWEB_CLUSTER_ARN="$CLUSTER_ARN"
done

# ── 6. Wait for rollouts ──────────────────────────────────────────────────────
echo "==> waiting for services"
kubectl wait --for=condition=ready pod -l app=spicedb --timeout=180s || true
for d in registry identity-server sample-api sample-api-a sample-api-b \
         sample-api-global agent-operator orchestrator dashboard; do
  if ! kubectl rollout status "deployment/$d" --timeout=180s; then
    echo "  ERROR: $d did not become Ready. Recent logs:" >&2
    kubectl logs "deployment/$d" --tail=25 2>&1 | sed 's/^/    /' >&2
    echo "  (also: kubectl describe pod -l app=$d | tail -20)" >&2
    exit 1
  fi
done

# ── 7. Seed templates (ECR-qualified agent images) ────────────────────────────
echo "==> seeding templates"
ECR_REGISTRY="$ECR" IMAGE_TAG="$TAG" deploy/aws/seed-aws.sh

# ── 8. Public ingress + OIDC origin (only when the DNS root is set up) ─────────
# Provisions the ALB (via the AWS LB Controller, installed by up.sh) and routes
# spawnly.run/auth.spawnly.run, then wires the browser-facing OIDC origin.
CERT_ARN="$(terraform -chdir=deploy/aws/dns output -raw acm_certificate_arn 2>/dev/null || true)"
if [ -n "$CERT_ARN" ] && [ "$CERT_ARN" != "None" ]; then
  echo "==> applying public ingress (ACM cert)"
  # Render the cert into the manifest so the object is complete on first reconcile
  # (avoids the LB Controller building the 443 listener before the cert lands).
  sed "s|\${CERT_ARN}|${CERT_ARN}|g" deploy/aws/ingress.yaml | kubectl apply -f -

  # Public OIDC origin (Phase 4). Single-origin design: the browser only talks to
  # the apex, which reverse-proxies /connect,/.well-known,/Account to
  # identity-server. So OIDC_AUTHORITY (dashboard's authorize redirect + its
  # client's redirect_uri) and DASHBOARD_ORIGIN (the IdP's allowed redirect/
  # post-logout URIs for that client) must both be the public https origin —
  # while ISSUER_URI / IDENTITY_INTERNAL_URL stay in-cluster (the resource
  # servers validate `iss`). FORWARDED_HEADERS lets the IdP honor the ALB's
  # X-Forwarded-Proto so its cookies are Secure; the dashboard reads that header
  # directly for its own cookies.
  PUBLIC_ORIGIN="https://${PUBLIC_DOMAIN:-spawnly.run}"
  echo "==> wiring public OIDC origin ($PUBLIC_ORIGIN)"
  kubectl set env deployment/dashboard OIDC_AUTHORITY="$PUBLIC_ORIGIN"
  kubectl set env deployment/identity-server DASHBOARD_ORIGIN="$PUBLIC_ORIGIN" FORWARDED_HEADERS=true
  kubectl rollout status deployment/dashboard --timeout=120s
  kubectl rollout status deployment/identity-server --timeout=120s
else
  echo "==> skipping public ingress (deploy/aws/dns not applied — see its README)"
fi

echo ""
echo "Deploy complete. Port-forward the dashboard:  kubectl port-forward svc/dashboard 8090:8080"
