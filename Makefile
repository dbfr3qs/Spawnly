# Makefile
MODULE        := github.com/agent-platform/poc
KIND_CLUSTER  := agent-platform
IMAGE_TAG     := latest
SERVICES      := operator orchestrator registry sample-api agent dashboard

.PHONY: build test docker-build kind-up kind-down kind-load spire deploy bootstrap demo

build:
	@for svc in $(SERVICES); do \
		echo "Building $$svc..."; \
		go build -o bin/$$svc ./cmd/$$svc; \
	done

test:
	go test ./... -v -count=1

docker-build:
	@for svc in $(SERVICES); do \
		echo "Building image agent-$$svc:$(IMAGE_TAG)..."; \
		docker build --target $$svc -t agent-$$svc:$(IMAGE_TAG) .; \
	done
	docker build --target identity-server -t agent-identity-server:$(IMAGE_TAG) .

kind-up:
	kind create cluster --name $(KIND_CLUSTER) --config deploy/kind/cluster.yaml

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

kind-load: docker-build
	@for svc in $(SERVICES); do \
		kind load docker-image agent-$$svc:$(IMAGE_TAG) --name $(KIND_CLUSTER); \
	done
	kind load docker-image agent-identity-server:$(IMAGE_TAG) --name $(KIND_CLUSTER)

spire:
	helm repo add spiffe https://spiffe.github.io/helm-charts-hardened/ 2>/dev/null || true
	helm repo update
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

bootstrap:
	./scripts/bootstrap.sh

demo:
	./scripts/demo.sh
