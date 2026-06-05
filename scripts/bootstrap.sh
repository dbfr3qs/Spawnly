#!/usr/bin/env bash
# scripts/bootstrap.sh
set -euo pipefail

KIND_CLUSTER="agent-platform"
IMAGE_TAG="latest"

# ── Env file ───────────────────────────────────────────────────────────────────
# Load a gitignored .env at the repo root so secrets (e.g. ANTHROPIC_API_KEY)
# don't have to live in your shell. Values in .env override the current shell.
# Set ENV_FILE to point elsewhere, or delete .env to fall back to the shell env.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env}"
if [ -f "$ENV_FILE" ]; then
  echo "==> Loading env from ${ENV_FILE}"
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

# ── Cluster ──────────────────────────────────────────────────────────────────

if kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER}$"; then
  echo "==> Kind cluster '${KIND_CLUSTER}' already exists — skipping creation"
else
  echo "==> Creating Kind cluster..."
  kind create cluster --name "$KIND_CLUSTER" --config deploy/kind/cluster.yaml
fi

# Inside a devcontainer, 127.0.0.1 can't reach the Kind control plane, so we join
# Kind's docker network and point kubectl at the control-plane container IP. On a
# native host (macOS/Linux) Kind's default 127.0.0.1 kubeconfig already works, so
# we skip this entirely. Override detection with BOOTSTRAP_IN_CONTAINER=1|0.
IN_CONTAINER="${BOOTSTRAP_IN_CONTAINER:-}"
if [ -z "$IN_CONTAINER" ]; then
  if [ -f /.dockerenv ]; then IN_CONTAINER=1; else IN_CONTAINER=0; fi
fi

if [ "$IN_CONTAINER" = "1" ]; then
  echo "==> Connecting devcontainer to Kind network..."
  CONTAINER_ID=$(cat /etc/hostname)
  docker network connect kind "$CONTAINER_ID" 2>/dev/null || true
  # Point kubectl at the control plane container IP (127.0.0.1 is unreachable inside a devcontainer)
  CONTROL_PLANE_IP=$(docker inspect "${KIND_CLUSTER}-control-plane" \
    --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}')
  kubectl config set-cluster "kind-${KIND_CLUSTER}" --server="https://${CONTROL_PLANE_IP}:6443"
else
  echo "==> Native host detected — using Kind's default kubeconfig (127.0.0.1)."
  # Self-correct a kubeconfig left pointing at a container IP from a previous in-container run.
  kind export kubeconfig --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
fi

# ── Images ───────────────────────────────────────────────────────────────────

echo "==> Building Docker images..."
# --load forces Buildx to place the result in the local Docker image store. Some
# setups default to a docker-container builder that only writes to its own build
# cache, so a plain `docker build` "succeeds" but the tag never lands locally and
# `kind load docker-image` later fails with "not present locally". After each
# build we assert the tag is actually present, so a silent miss fails loudly here
# with a fix hint rather than at the load step.
build_image() {
  local target="$1" tag="$2"
  docker build --load --target "$target" -t "$tag" .
  if ! docker image inspect "$tag" >/dev/null 2>&1; then
    echo "ERROR: '$tag' built but not found in the local Docker image store." >&2
    echo "       Your active Buildx builder likely doesn't load into the image store." >&2
    echo "       Fix: docker buildx use desktop-linux   (or: docker buildx use default)" >&2
    exit 1
  fi
}

for svc in operator orchestrator registry sample-api go-worker dashboard child-agent parent-agent; do
  build_image "$svc" "agent-$svc:$IMAGE_TAG"
done
# agent-sidecar is special: its stage is `agent-sidecar` and the operator
# references the image as `agent-sidecar:latest`, not `agent-agent-sidecar`.
build_image agent-sidecar "agent-sidecar:$IMAGE_TAG"
build_image identity-server "agent-identity-server:$IMAGE_TAG"
build_image weather-monitor "agent-weather-monitor:$IMAGE_TAG"

echo "==> Loading images into Kind..."
for svc in operator orchestrator registry sample-api go-worker dashboard identity-server child-agent parent-agent; do
  kind load docker-image "agent-$svc:$IMAGE_TAG" --name "$KIND_CLUSTER"
done
kind load docker-image "agent-sidecar:$IMAGE_TAG" --name "$KIND_CLUSTER"
kind load docker-image "agent-weather-monitor:$IMAGE_TAG" --name "$KIND_CLUSTER"

# ── SPIRE ────────────────────────────────────────────────────────────────────

echo "==> Installing SPIRE via Helm..."
helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ 2>/dev/null || true
helm repo update spiffe 2>/dev/null || echo "  Warning: could not refresh spiffe chart index, using cached version"
helm upgrade --install spire-crds spiffe/spire-crds \
  --namespace spire-system --create-namespace --wait

helm upgrade --install spire spiffe/spire \
  --namespace spire-system \
  --values deploy/spire/values.yaml \
  --wait --timeout=5m

# Force-delete any SPIRE pods stuck in Unknown state (can happen after node restart).
# The agent pod is restarted to ensure it reads the freshly-synced trust bundle.
echo "==> Recovering any stuck SPIRE pods..."
UNKNOWN_PODS=$(kubectl -n spire-system get pods --field-selector=status.phase=Unknown \
  -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)
for pod in $UNKNOWN_PODS; do
  echo "  Force-deleting stuck pod: $pod"
  kubectl -n spire-system delete pod "$pod" --force --grace-period=0 2>/dev/null || true
done
# Restart the SPIRE agent so it reads the current trust bundle from the ConfigMap
# (needed when server restarts rotate the CA before the agent can sync).
kubectl -n spire-system rollout restart daemonset/spire-agent 2>/dev/null || true

echo "==> Waiting for SPIRE OIDC discovery provider..."
kubectl -n spire-system wait --for=condition=available \
  deployment/spire-spiffe-oidc-discovery-provider --timeout=120s

echo "==> Applying ClusterSPIFFEID..."
kubectl apply -f deploy/spire/clusterspiffeid.yaml

# ── Manifests ────────────────────────────────────────────────────────────────

echo "==> Applying CRD..."
kubectl apply -f deploy/crds/agentworkload.yaml

echo "==> Deploying services..."
kubectl apply -f deploy/manifests/rbac.yaml
kubectl apply -f deploy/manifests/spicedb.yaml
kubectl apply -f deploy/manifests/registry.yaml
kubectl apply -f deploy/manifests/identityserver.yaml
kubectl apply -f deploy/manifests/sample-api.yaml
kubectl apply -f deploy/manifests/operator.yaml
kubectl apply -f deploy/manifests/orchestrator.yaml
kubectl apply -f deploy/manifests/dashboard.yaml

echo "==> Waiting for all services to be ready..."
kubectl wait --for=condition=ready pod -l app=spicedb --timeout=120s
kubectl wait --for=condition=available deployment/registry --timeout=120s
kubectl wait --for=condition=available deployment/identity-server --timeout=120s
kubectl wait --for=condition=available deployment/sample-api --timeout=120s
kubectl wait --for=condition=available deployment/agent-operator --timeout=120s
kubectl wait --for=condition=available deployment/orchestrator --timeout=120s
kubectl wait --for=condition=available deployment/dashboard --timeout=120s

# ── Secrets ──────────────────────────────────────────────────────────────────

echo "==> Creating AI provider secret..."
# Resolve provider, key, and model from env — supports Anthropic and OpenAI out of the box.
_AI_PROVIDER="${AI_PROVIDER:-anthropic}"
if [ "$_AI_PROVIDER" = "openai" ]; then
  _AI_API_KEY="${AI_API_KEY:-${OPENAI_API_KEY:-}}"
  _AI_MODEL="${AI_MODEL:-openai/gpt-4o}"
else
  _AI_API_KEY="${AI_API_KEY:-${ANTHROPIC_API_KEY:-}}"
  _AI_MODEL="${AI_MODEL:-anthropic/claude-sonnet-4-6}"
fi
kubectl create secret generic ai-provider \
  --from-literal=provider="${_AI_PROVIDER}" \
  --from-literal=api-key="${_AI_API_KEY}" \
  --from-literal=model="${_AI_MODEL}" \
  --dry-run=client -o yaml | kubectl apply -f -
echo "  ai-provider secret applied (provider=${_AI_PROVIDER}, model=${_AI_MODEL})"

# ── Templates ────────────────────────────────────────────────────────────────
# Always re-seed — the registry store is in-memory and resets on every restart.
# Delegates to seed.sh, which discovers every co-located template.json.

echo "==> Seeding agent templates..."
"${REPO_ROOT}/scripts/seed.sh"

echo ""
echo "Bootstrap complete."
echo ""
echo "Run the interactive demo:  ./scripts/demo.sh"
echo "This will port-forward the dashboard to http://localhost:8090"
