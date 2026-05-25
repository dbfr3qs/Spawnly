#!/usr/bin/env bash
# scripts/bootstrap.sh
set -euo pipefail

KIND_CLUSTER="agent-platform"
IMAGE_TAG="latest"

echo "==> Creating Kind cluster..."
kind create cluster --name "$KIND_CLUSTER" --config deploy/kind/cluster.yaml

echo "==> Connecting devcontainer to Kind network..."
CONTAINER_ID=$(cat /etc/hostname)
docker network connect kind "$CONTAINER_ID" 2>/dev/null || true
# Point kubectl at the control plane container IP (127.0.0.1 is unreachable from inside a devcontainer)
CONTROL_PLANE_IP=$(docker inspect "${KIND_CLUSTER}-control-plane" \
  --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}')
kubectl config set-cluster "kind-${KIND_CLUSTER}" --server="https://${CONTROL_PLANE_IP}:6443"

echo "==> Building Docker images..."
for svc in operator orchestrator registry sample-api agent dashboard; do
  docker build --target "$svc" -t "agent-$svc:$IMAGE_TAG" .
done
docker build --target identity-server -t agent-identity-server:$IMAGE_TAG .

echo "==> Loading images into Kind..."
for svc in operator orchestrator registry sample-api agent dashboard identity-server; do
  kind load docker-image "agent-$svc:$IMAGE_TAG" --name "$KIND_CLUSTER"
done

echo "==> Installing SPIRE via Helm..."
helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ 2>/dev/null || true
helm repo update
helm upgrade --install spire-crds spiffe/spire-crds \
  --namespace spire-system --create-namespace --wait

helm upgrade --install spire spiffe/spire \
  --namespace spire-system \
  --values deploy/spire/values.yaml \
  --wait --timeout=5m

echo "==> Waiting for SPIRE OIDC discovery provider..."
kubectl -n spire-system wait --for=condition=available \
  deployment/spire-spiffe-oidc-discovery-provider --timeout=120s

echo "==> Applying ClusterSPIFFEID..."
kubectl apply -f deploy/spire/clusterspiffeid.yaml

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

kill $PF_REG_SEED 2>/dev/null || true

echo ""
echo "Bootstrap complete."
echo ""
echo "Run the interactive demo:  ./scripts/demo.sh"
echo "This will port-forward the dashboard to http://localhost:8090"
