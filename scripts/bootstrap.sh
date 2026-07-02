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
  # ~/.kube/config is not a persisted mount, so a devcontainer reopen/rebuild
  # wipes it to an empty skeleton (no context → kubectl/helm fall back to
  # localhost:8080). `kind create cluster` is what normally writes the full
  # kubeconfig, but we skip creation here, so regenerate it explicitly. The
  # IN_CONTAINER block below then rewrites the server to the container IP.
  echo "==> Restoring kubeconfig from existing cluster..."
  kind export kubeconfig --name "$KIND_CLUSTER"
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

# Single source of truth: the Makefile's SERVICES list. Avoids drift between
# here and the Makefile (which previously left several agents unbuilt here).
SERVICES="$(make -s -C "$REPO_ROOT" print-SERVICES)"
for svc in $SERVICES; do
  # agent-sidecar is special: its stage is `agent-sidecar` and the operator
  # references the image as `agent-sidecar:latest`, not `agent-agent-sidecar`.
  [ "$svc" = "agent-sidecar" ] && continue
  build_image "$svc" "agent-$svc:$IMAGE_TAG"
done
build_image agent-sidecar "agent-sidecar:$IMAGE_TAG"

echo "==> Loading images into Kind..."
for svc in $SERVICES; do
  [ "$svc" = "agent-sidecar" ] && continue
  kind load docker-image "agent-$svc:$IMAGE_TAG" --name "$KIND_CLUSTER"
done
kind load docker-image "agent-sidecar:$IMAGE_TAG" --name "$KIND_CLUSTER"

# ── SPIRE ────────────────────────────────────────────────────────────────────

echo "==> Installing SPIRE via Helm..."
helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ 2>/dev/null || true
helm repo update spiffe 2>/dev/null || echo "  Warning: could not refresh spiffe chart index, using cached version"
helm upgrade --install spire-crds spiffe/spire-crds \
  --namespace spire-system --create-namespace --wait

# Install the SPIRE chart WITHOUT Helm's workload wait. Helm v4's default '--wait'
# ('watcher') strategy hangs indefinitely against this cluster — it never returns
# and ignores its own --timeout even when every pod is already Ready (a known v4
# regression). Omitting --wait falls back to the 'hookOnly' strategy: Helm still
# blocks on the chart's post-install hook job, then returns promptly. We gate
# actual workload readiness ourselves with `kubectl rollout status` below, which
# is reliable here. A coreutils `timeout` + one retry stay as a backstop in case
# even the apply/hook phase wedges (a killed install leaves the release in
# pending-install, so it must be uninstalled before the retry's upgrade, which
# would otherwise refuse with "another operation in progress").
install_spire() {
  timeout --kill-after=30s 360 \
    helm upgrade --install spire spiffe/spire \
      --namespace spire-system \
      --values deploy/spire/values.yaml \
      --timeout=5m
}

if ! install_spire; then
  echo "  SPIRE chart install did not finish; clearing the release and retrying once..."
  helm uninstall spire --namespace spire-system --timeout=90s 2>/dev/null || true
  if ! install_spire; then
    echo "  ERROR: SPIRE chart install failed twice. Inspect 'kubectl -n spire-system get pods' and 'helm list -A'." >&2
    exit 1
  fi
fi

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

# Gate on actual workload readiness ourselves (Helm v4's watcher can't be trusted
# to — see the install note above). This also waits for the agent we just
# restarted to come back up.
#
# Timeout is generous (10m): the SPIRE pods pull a busybox init image from Docker
# Hub on first start, and on a slow or flaky egress that pull can ImagePullBackOff
# for several minutes before succeeding. A short timeout here aborts the whole
# bootstrap over a transient pull, so we wait it out rather than fail fast.
echo "==> Waiting for SPIRE components to be ready (first start may pull images)..."
kubectl -n spire-system rollout status statefulset/spire-server --timeout=600s
kubectl -n spire-system rollout status daemonset/spiffe-csi-driver --timeout=600s
kubectl -n spire-system rollout status daemonset/spire-agent --timeout=600s
kubectl -n spire-system rollout status deployment/spire-spiffe-oidc-discovery-provider --timeout=600s

echo "==> Applying ClusterSPIFFEID..."
kubectl apply -f deploy/spire/clusterspiffeid.yaml

# ── Manifests ────────────────────────────────────────────────────────────────

echo "==> Applying CRD..."
kubectl apply -f deploy/crds/agentworkload.yaml

# Control-plane shared secret — gates the registry's template + consent
# control-plane endpoints (CONTROL_PLANE_AUTH=shared-secret). Created before the
# services so registry and orchestrator come up already enforcing it; both read
# it from the same `control-plane-auth` Secret, so their tokens match by
# construction. Reuse an existing token across re-bootstraps (keeps any seeded
# clients — seed.sh, a Terraform provider — working mid-session); generate one
# on first bootstrap.
echo "==> Ensuring control-plane auth secret..."
CP_TOKEN=$(kubectl get secret control-plane-auth -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null || true)
if [ -z "$CP_TOKEN" ]; then
  CP_TOKEN=$(openssl rand -hex 32)
  echo "  generated a new control-plane token"
else
  echo "  reusing existing control-plane token"
fi
kubectl create secret generic control-plane-auth \
  --from-literal=auth="shared-secret" \
  --from-literal=token="${CP_TOKEN}" \
  --dry-run=client -o yaml | kubectl apply -f -

# Interactive dashboard login (local demo): alice/alice (the ADMIN user, who
# can manage agent types), plus an optional non-admin viewer/viewer for
# exercising authz-deny paths and e2e. The IdP reads these from the
# `dashboard-user` Secret (optional secretKeyRefs); without the admin password,
# login is disabled (fail closed). The public AWS deploy generates a strong
# password and sets NO viewer so the internet-facing
# dashboard has no guessable login of either kind.
echo "==> Ensuring dashboard login secret (local demo: alice/alice admin + viewer/viewer non-admin)..."
kubectl create secret generic dashboard-user \
  --from-literal=username="alice" \
  --from-literal=password="alice" \
  --from-literal=viewerUsername="viewer" \
  --from-literal=viewerPassword="viewer" \
  --dry-run=client -o yaml | kubectl apply -f -

# Keycloak bootstrap admin (Phase 0). Keycloak runs NEXT TO identity-server; the
# realm (deploy/keycloak/) is applied by Terraform further below. The admin
# credential is generated here (never baked into the image) and reused across
# re-bootstraps so a mid-session `terraform apply` keeps working. On kind the
# store is the in-memory dev-file DB, so this admin is re-created from the Secret
# on every Keycloak restart.
echo "==> Ensuring keycloak-admin secret..."
KC_ADMIN_PASSWORD=$(kubectl get secret keycloak-admin -o jsonpath='{.data.password}' 2>/dev/null | base64 -d 2>/dev/null || true)
if [ -z "$KC_ADMIN_PASSWORD" ]; then
  KC_ADMIN_PASSWORD=$(openssl rand -hex 24)
  echo "  generated a new keycloak admin password"
else
  echo "  reusing existing keycloak admin password"
fi
kubectl create secret generic keycloak-admin \
  --from-literal=username="admin" \
  --from-literal=password="${KC_ADMIN_PASSWORD}" \
  --dry-run=client -o yaml | kubectl apply -f -

# travel-tools MCP server upstream keys (Duffel/LiteAPI), from mcp/travel-tools/.env
# — owned by that one service, kept out of the global .env. Created here so the
# travel-tools pod has them on first start; absent keys just disable that tool.
TT_ENV="$REPO_ROOT/mcp/travel-tools/.env"
echo "==> Ensuring travel-tools-secrets (Duffel/LiteAPI; from mcp/travel-tools/.env)..."
TT_DUFFEL="" TT_LITEAPI=""
if [ -f "$TT_ENV" ]; then
  TT_DUFFEL=$(grep -E '^DUFFEL_API_KEY=' "$TT_ENV" | head -1 | cut -d= -f2- | tr -d "\"' ")
  TT_LITEAPI=$(grep -E '^LITEAPI_KEY=' "$TT_ENV" | head -1 | cut -d= -f2- | tr -d "\"' ")
else
  echo "    (no mcp/travel-tools/.env — flights/hotels tools will report 'not configured')"
fi
kubectl create secret generic travel-tools-secrets \
  --from-literal=DUFFEL_API_KEY="$TT_DUFFEL" \
  --from-literal=LITEAPI_KEY="$TT_LITEAPI" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> Deploying services..."
kubectl apply -f deploy/manifests/rbac.yaml
kubectl apply -f deploy/manifests/spicedb.yaml
kubectl apply -f deploy/manifests/registry.yaml
kubectl apply -f deploy/manifests/identityserver.yaml
kubectl apply -f deploy/manifests/keycloak.yaml
kubectl apply -f deploy/manifests/sample-api-a.yaml
kubectl apply -f deploy/manifests/sample-api-b.yaml
kubectl apply -f deploy/manifests/travel-tools.yaml
kubectl apply -f deploy/manifests/operator.yaml
kubectl apply -f deploy/manifests/orchestrator.yaml
kubectl apply -f deploy/manifests/dashboard.yaml
# Ingress NetworkPolicies for registry/orchestrator/spicedb. Inert under kind's
# default kindnet (no NetworkPolicy enforcement) — see the file header.
kubectl apply -f deploy/manifests/networkpolicy.yaml

# On a pre-existing cluster the manifests are unchanged (image tags are
# `:latest`), so the applies above are no-ops and the running pods keep the
# PREVIOUSLY loaded images — a bootstrap would silently test stale code.
# Restart every platform deployment so the freshly kind-loaded images take
# effect (this also resets IdentityServer's in-memory grant store, dropping
# stale pending CIBA requests from earlier sessions).
echo "==> Restarting platform deployments to pick up freshly loaded images..."
kubectl rollout restart \
  deployment/registry deployment/identity-server deployment/keycloak \
  deployment/sample-api-a deployment/sample-api-b \
  deployment/travel-tools deployment/agent-operator \
  deployment/orchestrator deployment/dashboard

echo "==> Waiting for all services to be ready..."
kubectl wait --for=condition=ready pod -l app=spicedb --timeout=120s
kubectl rollout status deployment/registry --timeout=120s
kubectl rollout status deployment/identity-server --timeout=120s
# Keycloak's first start builds/imports the realm DB and can be slower than the
# .NET IdP, so give it a longer window.
kubectl rollout status deployment/keycloak --timeout=300s
kubectl rollout status deployment/sample-api-a --timeout=120s
kubectl rollout status deployment/sample-api-b --timeout=120s
kubectl rollout status deployment/travel-tools --timeout=120s
kubectl rollout status deployment/agent-operator --timeout=120s
kubectl rollout status deployment/orchestrator --timeout=120s
kubectl rollout status deployment/dashboard --timeout=120s

# ── Keycloak realm (Terraform) ─────────────────────────────────────────────────
# Declare the `spawnly` realm (clients/scopes/audiences/roles/users) with the
# keycloak/keycloak Terraform provider. Runs against a throwaway port-forward to
# Keycloak's admin API; all secrets are passed as TF vars (the generated admin
# password, the demo client secrets + alice/viewer passwords that match the
# `dashboard-user`/manifest values used by the resource servers). Nothing is
# committed. IdentityServer bring-up above is untouched — agents stay on it.
#
# NOTE: on kind, Keycloak uses the in-memory dev-file DB (deploy/manifests/
# keycloak.yaml), so a keycloak pod restart WIPES the realm — re-run bootstrap
# to re-apply it (Terraform re-creates clients/scopes/users idempotently).
echo "==> Applying Keycloak realm via Terraform..."
KC_TF_DIR="$REPO_ROOT/deploy/keycloak"
# Run the whole port-forward + apply in a SUBSHELL so its EXIT trap tears down
# the forward on ANY exit path (success, terraform failure, or set -e abort)
# without touching any trap the rest of bootstrap.sh relies on.
(
  # Forward to a LESS-COMMON local port (18099) to avoid colliding with some
  # other listener on the conventional 8080 — a stale listener would make curl
  # succeed against the WRONG process and terraform then apply against it.
  KC_LOCAL_PORT=18099
  kubectl port-forward svc/keycloak "${KC_LOCAL_PORT}:8080" >/dev/null 2>&1 &
  KC_PF_PID=$!
  trap 'kill "$KC_PF_PID" 2>/dev/null || true' EXIT

  # Wait for the admin API to answer through the forward, then FAIL LOUDLY if it
  # never did rather than letting the loop fall through into a doomed apply.
  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "http://localhost:${KC_LOCAL_PORT}/realms/master/.well-known/openid-configuration" 2>/dev/null; then
      break
    fi
    sleep 2
  done
  curl -fsS -o /dev/null "http://localhost:${KC_LOCAL_PORT}/realms/master/.well-known/openid-configuration" \
    || { echo "ERROR: Keycloak admin API not reachable on localhost:${KC_LOCAL_PORT} after 120s" >&2; exit 1; }

  # Pass every TF var via TF_VAR_* ENVIRONMENT rather than `-var` argv: CLI args
  # are visible in ps / /proc/<pid>/cmdline to any local user while terraform
  # runs, which would briefly expose the generated admin password and the demo
  # secrets. Environment is not exposed via ps here (and these are subshell-local
  # exports, so they don't leak into the rest of bootstrap.sh).
  export TF_VAR_keycloak_url="http://localhost:${KC_LOCAL_PORT}"
  export TF_VAR_keycloak_admin_username="admin"
  export TF_VAR_keycloak_admin_password="${KC_ADMIN_PASSWORD}"
  export TF_VAR_dashboard_origin="http://localhost:8090"
  export TF_VAR_dashboard_client_secret="dashboard-secret"
  export TF_VAR_orchestrator_client_secret="orchestrator-secret"
  export TF_VAR_idp_consent_client_secret="idp-consent-secret"
  export TF_VAR_machine_client_secret="placeholder"
  export TF_VAR_alice_password="alice"
  export TF_VAR_viewer_password="viewer"

  terraform -chdir="$KC_TF_DIR" init -input=false >/dev/null
  terraform -chdir="$KC_TF_DIR" apply -input=false -auto-approve
)
echo "  Keycloak realm applied (spawnly)."

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
