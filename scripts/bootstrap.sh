#!/usr/bin/env bash
# scripts/bootstrap.sh
set -euo pipefail

KIND_CLUSTER="agent-platform"
IMAGE_TAG="latest"

# ── Cluster ──────────────────────────────────────────────────────────────────

if kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER}$"; then
  echo "==> Kind cluster '${KIND_CLUSTER}' already exists — skipping creation"
else
  echo "==> Creating Kind cluster..."
  kind create cluster --name "$KIND_CLUSTER" --config deploy/kind/cluster.yaml
fi

echo "==> Connecting devcontainer to Kind network..."
CONTAINER_ID=$(cat /etc/hostname)
docker network connect kind "$CONTAINER_ID" 2>/dev/null || true
# Point kubectl at the control plane container IP (127.0.0.1 is unreachable inside a devcontainer)
CONTROL_PLANE_IP=$(docker inspect "${KIND_CLUSTER}-control-plane" \
  --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}')
kubectl config set-cluster "kind-${KIND_CLUSTER}" --server="https://${CONTROL_PLANE_IP}:6443"

# ── Images ───────────────────────────────────────────────────────────────────

echo "==> Building Docker images..."
for svc in operator orchestrator registry sample-api agent dashboard agent-sidecar child-agent parent-agent; do
  docker build --target "$svc" -t "agent-$svc:$IMAGE_TAG" .
done
docker build --target identity-server -t agent-identity-server:$IMAGE_TAG .
docker build --target weather-monitor -t "agent-weather-monitor:$IMAGE_TAG" .

echo "==> Loading images into Kind..."
for svc in operator orchestrator registry sample-api agent dashboard agent-sidecar identity-server child-agent parent-agent; do
  kind load docker-image "agent-$svc:$IMAGE_TAG" --name "$KIND_CLUSTER"
done
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

echo "==> Seeding agent templates..."
kubectl port-forward svc/registry 18080:8080 &
PF_REG_SEED=$!
sleep 2

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "worker",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Generic Worker", "description": "Calls the sample API and exits"},
    "runtimeSpec": {
      "image": "agent-agent:latest",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {"LOG_LEVEL": "info", "MAX_RETRIES": "3"}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  worker template seeded"

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "weather-monitor",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Weather Monitor", "description": "Long-lived agent that monitors weather on a loop"},
    "runtimeSpec": {
      "image": "agent-weather-monitor:latest",
      "lifecycle": "long-lived",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {"LOG_LEVEL": "info"}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  weather-monitor template seeded"

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "parent-agent",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Parent Agent", "description": "Spawns a child agent, retrieves a random string via A2A, then exits"},
    "runtimeSpec": {
      "image": "agent-parent-agent:latest",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  parent-agent template seeded"

curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "agentType": "child-agent",
    "version": "1.0.0",
    "status": "active",
    "meta": {"displayName": "Child Agent", "description": "Long-lived A2A server that generates a random string on request"},
    "runtimeSpec": {
      "image": "agent-child-agent:latest",
      "lifecycle": "long-lived",
      "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
      "envDefaults": {}
    },
    "authzTemplate": {
      "spiceDbRelations": [
        {"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
      ]
    }
  }'
echo "  child-agent template seeded"

kill $PF_REG_SEED 2>/dev/null || true

echo ""
echo "Bootstrap complete."
echo ""
echo "Run the interactive demo:  ./scripts/demo.sh"
echo "This will port-forward the dashboard to http://localhost:8090"
