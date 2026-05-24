# Makefile
MODULE        := github.com/agent-platform/poc
KIND_CLUSTER  := agent-platform
IMAGE_TAG     := latest
SERVICES      := operator orchestrator registry sample-api agent

.PHONY: build test docker-build kind-up kind-down kind-load deploy bootstrap demo

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

deploy:
	kubectl apply -f deploy/crds/
	kubectl apply -f deploy/manifests/

bootstrap:
	./scripts/bootstrap.sh

demo:
	./scripts/demo.sh
