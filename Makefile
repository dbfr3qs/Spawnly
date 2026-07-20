# Makefile
MODULE        := github.com/spawnly/platform
KIND_CLUSTER  := agent-platform
IMAGE_TAG     := latest
GO_SERVICES   := operator orchestrator registry sample-api agent-sidecar dashboard mobile-gateway substrate-agent
# Separate-module Go agents: their own go.mod (not the root module), so they
# build via `cd agents/<name> && go build .` and map to image agent-<name>.
# (None at present — listed here so the build/Docker plumbing stays in place.)
GO_MODULE_AGENTS :=
NODE_AGENTS   := chain-worker travel-specialist travel-planner
# Demo agents built from their own Dockerfile stage. They follow the standard
# agent-<name> image convention (unlike agent-sidecar), so they need no special
# casing — they just weren't in any list before.
EXTRA_AGENTS  := keycloak weather-monitor
# MCP servers: scope-enforcing resource servers exposing real-upstream tools to
# agents (not agents themselves). Built from their own Dockerfile stage as
# agent-<name>, same convention as the rest.
MCP_SERVERS   := travel-tools
SERVICES      := $(GO_SERVICES) $(GO_MODULE_AGENTS) $(NODE_AGENTS) $(EXTRA_AGENTS) $(MCP_SERVERS)

.PHONY: build test test-provider test-dynamodb docker-build kind-up kind-down kind-load spire deploy bootstrap demo port-forward mailpit kubeconfig dash redeploy-% reload-% reload-sidecar logs-% reseed reseed-orgs print-% e2e-setup e2e-anchor-setup e2e e2e-subdomains mobile-dev tls-mimic tls-mimic-down substrate-ca substrate-setup substrate-verify substrate-down substrate-e2e-setup substrate-wire-entry-api substrate-isolation

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

# test-dynamodb — run the DynamoDB-backed Store (and, from Phase 2, DeviceStore)
# conformance suites against a throwaway amazon/dynamodb-local container. These
# tests are gated on DYNAMODB_TEST_ENDPOINT, so a plain `make test` skips them;
# this target supplies the container + endpoint. Needs docker. dynamodb-local
# ignores credentials, but the AWS SDK still signs requests, so dummy creds are
# provided — no real secrets, so this stays fork-safe in CI lane 1.
test-dynamodb:
	@docker rm -f spawnly-dynamodb-test >/dev/null 2>&1 || true
	docker run -d --name spawnly-dynamodb-test -p 8000:8000 amazon/dynamodb-local >/dev/null
	@echo "Waiting for dynamodb-local..."
	@for i in $$(seq 1 30); do curl -s -o /dev/null http://localhost:8000 && break; sleep 0.5; done
	@trap 'docker rm -f spawnly-dynamodb-test >/dev/null 2>&1 || true' EXIT; \
	 DYNAMODB_TEST_ENDPOINT=http://localhost:8000 \
	 AWS_ACCESS_KEY_ID=dummy AWS_SECRET_ACCESS_KEY=dummy AWS_REGION=us-east-1 \
	 go test ./internal/registry/... ./internal/mobilegateway/... -count=1

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
	# remote-substrate (Phase 2): ensure the throwaway dev CA Secret and enable the
	# x509pop node attestor ALONGSIDE k8s_psat (idempotent; platform pods untouched).
	./scripts/substrate-spire-x509pop.sh

deploy: spire
	kubectl apply -k deploy/crds/
	kubectl apply -k deploy/manifests/

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

# reseed-orgs — re-create the demo organizations (acme, globex) through the
# registry's onboarding API (Keycloak + SpiceDB dual-write). Idempotent; run
# after a registry redeploy that lost its in-memory org records.
reseed-orgs:
	./scripts/reseed-orgs.sh

port-forward:
	@echo "Dashboard → http://localhost:8090  (Ctrl+C to stop)"
	@while true; do kubectl port-forward svc/dashboard 8090:8080; sleep 1; done

# mailpit — port-forward the dev mail sink's web inbox so local signup
# email-verification mail is viewable at http://localhost:8025. Dev sends through
# the in-cluster Mailpit sink (not SES); 8025 is forwarded to the host in
# .devcontainer/devcontainer.json. Reconnecting loop survives brief drops.
mailpit:
	@echo "Mailpit inbox → http://localhost:8025  (Ctrl+C to stop)"
	@while true; do kubectl port-forward svc/mailpit 8025:8025; sleep 1; done

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

# mobile-dev — one command to run the mobile app against a local cluster
# (Tier A: SSE, no cloud push): deploy/reseed the gateway if needed, LAN-bound
# port-forwards, generate mobile/.env.local for this host's IP, and start Expo.
# Run on the HOST (it needs the LAN IP + firewall). Ctrl+C tears it down.
# See plans/mobile-ciba/testing-plan.md for the one-time device-build setup.
mobile-dev:
	./scripts/mobile-dev.sh

demo:
	./scripts/demo.sh

# e2e-setup — one-time install of Playwright and its Chromium browser for the
# dashboard UI test suite. Run after `make bootstrap`.
e2e-setup:
	cd e2e && npm install && npx playwright install --with-deps chromium

# e2e-anchor-setup — deploy the E2E-ONLY fake STS stub and point the registry's
# STS trust at it, so the anchor suite (anchor.spec.ts) can activate anchors
# in-cluster. Idempotent; NOT part of the kustomize base or bootstrap (see
# scripts/deploy-fake-sts.sh for the "e2e only" stance).
e2e-anchor-setup:
	./scripts/deploy-fake-sts.sh

# e2e — run the browser-based dashboard tests. Assumes a bootstrapped cluster
# (make bootstrap) and ANTHROPIC_API_KEY in .env. Playwright owns the dashboard
# port-forward (see e2e/playwright.config.ts → scripts/e2e.sh). Deploys fake-sts
# first so the anchor suite has an STS to prove control against.
e2e: e2e-anchor-setup
	cd e2e && npm test

# ── org-subdomains local TLS mimic (feature: org-subdomains, Phase 3) ─────────
# tls-mimic stands up full-fidelity HTTPS for the apex + satellites over the
# running kind stack so the whole subdomain flow works in a browser:
#     https://localtest.me         → apex (Global / picker / signup + KC proxy)
#     https://<slug>.localtest.me  → org satellite (scoped dashboard)
# A host-side Caddy container terminates TLS (its own internal CA — no host
# installs) and the cluster is flipped into apex=localtest.me issuer mode.
#
# MODE-SWITCH — MUTUALLY EXCLUSIVE with the default localhost:8090 loop
# (`make dash` / `make e2e`). Keycloak's issuer is a single string, so the
# cluster serves EITHER origin at a time. Opt in with `make tls-mimic`; ALWAYS
# return to the default mode with `make tls-mimic-down` before running the
# standard e2e suite. Both directions are idempotent and reversible.
tls-mimic:
	./scripts/local-tls.sh up

tls-mimic-down:
	./scripts/local-tls.sh down

# e2e-subdomains — the org-subdomains Playwright suite (Phase 4). Brings up the
# Phase-3 TLS mimic (make tls-mimic), then runs the `subdomains` project against
# https://<slug>.localtest.me. This is a LOCAL gate ONLY — like the weekly-cron
# `make e2e`, it is deliberately NOT wired into per-push CI.
#
# MODE-SWITCH: this flips the cluster into apex=localtest.me mode (same as
# `make tls-mimic`) and MUTUALLY EXCLUSIVE with the default `make e2e`. The
# harness is left UP for iteration — return to the default localhost:8090 mode
# (and the standard `make e2e`) with `make tls-mimic-down` when you're done.
e2e-subdomains: tls-mimic
	cd e2e && SUBDOMAIN_E2E=1 npx playwright test --project=subdomains
	@echo ""
	@echo "==> subdomains suite done. The TLS mimic is STILL UP (apex=localtest.me)."
	@echo "    Return to the default localhost:8090 mode with:  make tls-mimic-down"

# ── remote-substrate local sim (feature: remote-substrate, Phase 2) ───────────
# substrate-ca — ensure ONLY the throwaway x509pop dev CA Secret exists (no server
# changes). `make spire`/bootstrap already do this + enable x509pop; this is here
# for standalone use.
substrate-ca:
	./scripts/substrate-ca.sh

# substrate-setup — stand up the simulated substrate (deploy/substrate overlay:
# SPIRE agent w/ x509pop, SPIFFE CSI driver, operator) for one org (default acme)
# on a bootstrapped cluster: enables x509pop on the server, issues the org leaf,
# copies the trust bundle, applies the overlay. Idempotent.
#   make substrate-setup            # org=acme
#   make substrate-setup ORG=globex
substrate-setup:
	./scripts/substrate-setup.sh $(ORG)

# substrate-verify — prove the Phase-2 identity loop live (node under org path,
# pod gets org-scoped SVID verified by the Phase-1 Go verifier, forged-CA
# rejected). Run after substrate-setup. Wired for the Phase-6 harness / e2e.
substrate-verify:
	./scripts/substrate-verify.sh $(ORG)

# substrate-e2e-setup — the FIRST-CLASS Phase-6 harness: stand up a LIVE two-org
# substrate sim so the substrate Playwright specs run end-to-end instead of
# skipping. org A (default acme) is a fully-live substrate (dev CA + x509pop +
# overlay + the Entry API delegated-admin socket wired for dispatch/channel); org B
# (default globex) gets credentials only, for the isolation-matrix negatives. e2e
# only, idempotent, reversible with `make substrate-down`. Assumes a bootstrapped
# cluster + demo orgs (make bootstrap + make e2e-anchor-setup).
#   make substrate-e2e-setup                              # A=acme, B=globex
#   make substrate-e2e-setup SUBSTRATE_ORG_A=acme SUBSTRATE_ORG_B=globex
substrate-e2e-setup:
	./scripts/substrate-e2e-setup.sh

# substrate-wire-entry-api — wire ONLY the Entry API delegated-admin socket (shared
# hostPath between spire-server + orchestrator) so connect activates the dispatch
# channel. Folded into substrate-e2e-setup; here for standalone use / re-wiring.
substrate-wire-entry-api:
	./scripts/substrate-wire-entry-api.sh

# substrate-isolation — run the LIVE two-org isolation matrix (AC 1/4/5/6) after
# substrate-e2e-setup: real SVIDs through the real verifier, node→org binding,
# forged-CA rejection. The shell half of substrate-isolation.spec.ts.
#   make substrate-isolation                                   # A=acme, B=globex
#   make substrate-isolation SUBSTRATE_ORG_A=acme SUBSTRATE_ORG_B=globex
substrate-isolation:
	./scripts/substrate-isolation.sh

# substrate-down — remove the substrate sim overlay + per-run pods + org-B creds
# (leaves the dev CA Secret, the server's x509pop attestor, and the Entry API
# socket in place; re-run substrate-e2e-setup to bring back). SUBSTRATE_UNWIRE=1
# also unwires the Entry API socket back to seed.
substrate-down:
	./scripts/substrate-down.sh
