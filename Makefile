# Makefile
MODULE        := github.com/spawnly/platform
KIND_CLUSTER  := agent-platform
IMAGE_TAG     := latest
GO_SERVICES   := operator orchestrator registry sample-api agent-sidecar dashboard mobile-gateway
# Separate-module Go agents: their own go.mod (not the root module), so they
# build via `cd agents/<name> && go build .` and map to image agent-<name>.
# (None at present — listed here so the build/Docker plumbing stays in place.)
GO_MODULE_AGENTS :=
NODE_AGENTS   := chain-worker travel-specialist travel-planner
# Demo agents built from their own Dockerfile stage. They follow the standard
# agent-<name> image convention (unlike agent-sidecar), so they need no special
# casing — they just weren't in any list before.
EXTRA_AGENTS  := identity-server weather-monitor
# MCP servers: scope-enforcing resource servers exposing real-upstream tools to
# agents (not agents themselves). Built from their own Dockerfile stage as
# agent-<name>, same convention as the rest.
MCP_SERVERS   := travel-tools
SERVICES      := $(GO_SERVICES) $(GO_MODULE_AGENTS) $(NODE_AGENTS) $(EXTRA_AGENTS) $(MCP_SERVERS)

.PHONY: build test test-provider docker-build kind-up kind-down kind-load spire deploy bootstrap demo port-forward kubeconfig dash redeploy-% reload-% reload-sidecar logs-% reseed print-% e2e-setup e2e

# print-<VAR> — echo a make variable so shell scripts can read the authoritative
# lists instead of re-declaring them. e.g. `make -s print-SERVICES`.
print-%:
	@echo '$($*)'

build:
	@for svc in $(GO_SERVICES); do \
		echo "Building $$svc..."; \
		go build -o bin/$$svc ./cmd/$$svc; \
	done
	@for svc in $(GO_MODULE_AGENTS); do \
		echo "Building $$svc..."; \
		(cd agents/$$svc && go build -o ../../bin/$$svc .); \
	done

test:
	go test ./... -v -count=1

# Full gate for terraform-provider-spawnly: fmt/vet/unit + acceptance + the
# seeded-template parity check, on an ephemeral SpiceDB+registry testbed
# (no kind/SPIRE). Needs docker + the terraform CLI. Mirrors the
# terraform-provider CI workflow.
test-provider:
	./scripts/test-provider.sh

docker-build:
	@for svc in $(filter-out agent-sidecar,$(SERVICES)); do \
		echo "Building image agent-$$svc:$(IMAGE_TAG)..."; \
		docker build --target $$svc -t agent-$$svc:$(IMAGE_TAG) .; \
	done
	# agent-sidecar is special: its stage is `agent-sidecar` and the operator
	# references the image as `agent-sidecar:latest`, so the generic agent-<svc>
	# convention (agent-agent-sidecar) does not apply.
	docker build --target agent-sidecar -t agent-sidecar:$(IMAGE_TAG) .

kind-up:
	kind create cluster --name $(KIND_CLUSTER) --config deploy/kind/cluster.yaml

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

kind-load: docker-build
	@for svc in $(filter-out agent-sidecar,$(SERVICES)); do \
		kind load docker-image agent-$$svc:$(IMAGE_TAG) --name $(KIND_CLUSTER); \
	done
	kind load docker-image agent-sidecar:$(IMAGE_TAG) --name $(KIND_CLUSTER)

spire:
	helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ 2>/dev/null || true
	helm repo update spiffe 2>/dev/null || true
	helm upgrade --install spire-crds spiffe/spire-crds \
		--namespace spire-system --create-namespace --wait
	helm upgrade --install spire spiffe/spire \
		--namespace spire-system \
		--values deploy/spire/values.yaml \
		--wait --timeout=5m
	kubectl -n spire-system wait --for=condition=available \
		deployment/spire-spiffe-oidc-discovery-provider --timeout=120s
	kubectl apply -f deploy/spire/clusterspiffeid.yaml

deploy: spire
	kubectl apply -f deploy/crds/
	kubectl apply -f deploy/manifests/

redeploy-%:
	docker build --target $* -t agent-$*:$(IMAGE_TAG) .
	kind load docker-image agent-$*:$(IMAGE_TAG) --name $(KIND_CLUSTER)
	@if [ "$*" = "operator" ]; then DEPLOY=agent-operator; else DEPLOY=$*; fi; \
	kubectl rollout restart deployment/$$DEPLOY && \
	kubectl rollout status deployment/$$DEPLOY --timeout=60s

# reload-% — rebuild + load an agent image (not a Deployment) into Kind.
# Use this for chain-worker, weather-monitor, travel-planner, etc.
# Compiles TypeScript first if the agent directory has a tsconfig.json.
# After running, spawn a new agent from the dashboard to pick up the new image.
reload-%:
	@if [ -f agents/$*/tsconfig.json ]; then \
		echo "==> Compiling TypeScript for $*..."; \
		cd agents/$* && npx tsc; \
	fi
	docker build --target $* -t agent-$*:$(IMAGE_TAG) .
	kind load docker-image agent-$*:$(IMAGE_TAG) --name $(KIND_CLUSTER)
	@echo ""
	@echo "  agent-$* reloaded. Spawn a new agent in the dashboard to test."

# reload-sidecar — explicit override of reload-%. The sidecar is special: its
# Dockerfile stage is `agent-sidecar` and the operator references the image as
# `agent-sidecar:latest` (internal/operator/reconciler.go), NOT the
# `agent-<name>` the generic convention would produce (which would mis-target
# `sidecar` or mis-name `agent-agent-sidecar`). The sidecar runs as an init
# container in every agent pod (not a Deployment), so freshly spawned agents
# pick up the reloaded image — nothing to roll.
reload-sidecar:
	docker build --target agent-sidecar -t agent-sidecar:$(IMAGE_TAG) .
	kind load docker-image agent-sidecar:$(IMAGE_TAG) --name $(KIND_CLUSTER)
	@echo ""
	@echo "  agent-sidecar:$(IMAGE_TAG) reloaded. Spawn a new agent to pick it up."

# logs-% — stream logs from the most recently spawned pod of a given agent type.
# Usage: make logs-chain-worker
logs-%:
	@POD=$$(kubectl get pods -l agent-type=$* --sort-by=.metadata.creationTimestamp \
	  -o jsonpath='{.items[-1].metadata.name}' 2>/dev/null); \
	if [ -z "$$POD" ]; then echo "No pod found for agent type $*"; exit 1; fi; \
	echo "Streaming logs from $$POD ..."; \
	kubectl logs "$$POD" -c agent -f

# reseed — re-seed agent templates into the running registry without rebuilding.
# Run this after redeploying the registry (which loses its in-memory state).
# Seeds every co-located template.json via scripts/seed.sh.
reseed:
	./scripts/seed.sh

port-forward:
	@echo "Dashboard → http://localhost:8090  (Ctrl+C to stop)"
	@while true; do kubectl port-forward svc/dashboard 8090:8080; sleep 1; done

# kubeconfig — repair the kubeconfig after a Docker/Kind restart. Restores the
# cluster CA and, inside a devcontainer (where 127.0.0.1 can't reach Kind),
# re-points kubectl at the control-plane container's current IP (which drifts
# across restarts). On a native host it just resets Kind's default kubeconfig.
kubeconfig:
	@kind export kubeconfig --name $(KIND_CLUSTER) >/dev/null 2>&1 || { echo "Kind cluster '$(KIND_CLUSTER)' not found — run 'make bootstrap'"; exit 1; }
	@if [ -f /.dockerenv ]; then \
	  docker network connect kind "$$(cat /etc/hostname)" 2>/dev/null || true; \
	  CPIP=$$(docker inspect $(KIND_CLUSTER)-control-plane --format '{{(index .NetworkSettings.Networks "kind").IPAddress}}'); \
	  kubectl config set-cluster kind-$(KIND_CLUSTER) --server="https://$$CPIP:6443" >/dev/null; \
	  echo "kubeconfig → control-plane $$CPIP (devcontainer)"; \
	else \
	  echo "kubeconfig → Kind default (native host)"; \
	fi

# dash — one command to (re)connect and open the dashboard: repair the
# kubeconfig, then reuse the port-forward target's loop so it survives brief
# drops. Run sequentially (do not invoke with -j): kubeconfig must finish before
# port-forward starts.
dash: kubeconfig port-forward

bootstrap:
	./scripts/bootstrap.sh

demo:
	./scripts/demo.sh

# e2e-setup — one-time install of Playwright and its Chromium browser for the
# dashboard UI test suite. Run after `make bootstrap`.
e2e-setup:
	cd e2e && npm install && npx playwright install --with-deps chromium

# e2e — run the browser-based dashboard tests. Assumes a bootstrapped cluster
# (make bootstrap) and ANTHROPIC_API_KEY in .env. Playwright owns the dashboard
# port-forward (see e2e/playwright.config.ts → scripts/e2e.sh).
e2e:
	cd e2e && npm test
