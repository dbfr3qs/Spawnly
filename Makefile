# Makefile
MODULE        := github.com/spawnly/platform
KIND_CLUSTER  := agent-platform
IMAGE_TAG     := latest
GO_SERVICES   := operator orchestrator registry sample-api agent-sidecar dashboard
# Separate-module Go agents: their own go.mod (not the root module), so they
# build via `cd agents/<name> && go build .` and map to image agent-<name>.
GO_MODULE_AGENTS := go-worker
NODE_AGENTS   := child-agent parent-agent currency-converter trip-planner
SERVICES      := $(GO_SERVICES) $(GO_MODULE_AGENTS) $(NODE_AGENTS)

.PHONY: build test docker-build kind-up kind-down kind-load spire deploy bootstrap demo port-forward redeploy-% reload-% reload-sidecar logs-% reseed

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

docker-build:
	@for svc in $(filter-out agent-sidecar,$(SERVICES)); do \
		echo "Building image agent-$$svc:$(IMAGE_TAG)..."; \
		docker build --target $$svc -t agent-$$svc:$(IMAGE_TAG) .; \
	done
	# agent-sidecar is special: its stage is `agent-sidecar` and the operator
	# references the image as `agent-sidecar:latest`, so the generic agent-<svc>
	# convention (agent-agent-sidecar) does not apply.
	docker build --target agent-sidecar -t agent-sidecar:$(IMAGE_TAG) .
	docker build --target identity-server -t agent-identity-server:$(IMAGE_TAG) .
	docker build --target weather-monitor -t agent-weather-monitor:$(IMAGE_TAG) .

kind-up:
	kind create cluster --name $(KIND_CLUSTER) --config deploy/kind/cluster.yaml

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

kind-load: docker-build
	@for svc in $(filter-out agent-sidecar,$(SERVICES)); do \
		kind load docker-image agent-$$svc:$(IMAGE_TAG) --name $(KIND_CLUSTER); \
	done
	kind load docker-image agent-sidecar:$(IMAGE_TAG) --name $(KIND_CLUSTER)
	kind load docker-image agent-identity-server:$(IMAGE_TAG) --name $(KIND_CLUSTER)
	kind load docker-image agent-weather-monitor:$(IMAGE_TAG) --name $(KIND_CLUSTER)

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
# Use this for parent-agent, child-agent, weather-monitor, etc.
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
# Usage: make logs-parent-agent
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

bootstrap:
	./scripts/bootstrap.sh

demo:
	./scripts/demo.sh
