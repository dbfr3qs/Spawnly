FROM golang:1.25-alpine AS builder
WORKDIR /app
# GOWORK=off keeps the platform service builds (operator, orchestrator,
# registry, sample-api, dashboard, agent-sidecar) in pure root-module mode,
# unaffected by the go.work file that `COPY . .` pulls into the image.
ENV GOWORK=off
COPY go.mod go.sum ./
RUN go mod download
COPY . .

FROM builder AS build-operator
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/operator ./cmd/operator

FROM builder AS build-orchestrator
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/orchestrator ./cmd/orchestrator

FROM builder AS build-registry
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/registry ./cmd/registry

FROM builder AS build-sample-api
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/sample-api ./cmd/sample-api

FROM gcr.io/distroless/static-debian12 AS operator
COPY --from=build-operator /bin/operator /
ENTRYPOINT ["/operator"]

FROM gcr.io/distroless/static-debian12 AS orchestrator
COPY --from=build-orchestrator /bin/orchestrator /
ENTRYPOINT ["/orchestrator"]

FROM gcr.io/distroless/static-debian12 AS registry
COPY --from=build-registry /bin/registry /
ENTRYPOINT ["/registry"]

FROM gcr.io/distroless/static-debian12 AS sample-api
COPY --from=build-sample-api /bin/sample-api /
ENTRYPOINT ["/sample-api"]

# fake-sts is an E2E-ONLY trust-injection stub (see cmd/fake-sts/main.go). It has
# its own image stage, deliberately NOT in the Makefile SERVICES list nor the
# kustomize base, so a normal build/deploy can never pull it in — only the e2e
# path (scripts/deploy-fake-sts.sh) builds, loads, and applies it.
FROM builder AS build-fake-sts
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/fake-sts ./cmd/fake-sts

FROM gcr.io/distroless/static-debian12 AS fake-sts
COPY --from=build-fake-sts /bin/fake-sts /
ENTRYPOINT ["/fake-sts"]

# remote-demo — E2E-ONLY minimal consumer-hosted agent (multi-tenancy Phase 10).
# It only mints from the sidecar on a loop and posts mint_ok/mint_failed events,
# so the e2e can observe a remote agent's lifecycle parity. Not a real workload.
FROM builder AS build-remote-demo
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/remote-demo ./cmd/remote-demo

FROM gcr.io/distroless/static-debian12 AS remote-demo
COPY --from=build-remote-demo /bin/remote-demo /
ENTRYPOINT ["/remote-demo"]

FROM builder AS build-dashboard
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/dashboard ./cmd/dashboard

FROM gcr.io/distroless/static-debian12 AS dashboard
COPY --from=build-dashboard /bin/dashboard /
ENTRYPOINT ["/dashboard"]

FROM builder AS build-agent-sidecar
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/agent-sidecar ./cmd/agent-sidecar

FROM gcr.io/distroless/static-debian12 AS agent-sidecar
COPY --from=build-agent-sidecar /bin/agent-sidecar /
ENTRYPOINT ["/agent-sidecar"]

FROM builder AS build-mobile-gateway
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/mobile-gateway ./cmd/mobile-gateway

FROM gcr.io/distroless/static-debian12 AS mobile-gateway
COPY --from=build-mobile-gateway /bin/mobile-gateway /
ENTRYPOINT ["/mobile-gateway"]

# Shared SDK build — compiles @spawnly/sdk from source so the dist is
# reproducible (agents/*/dist is gitignored). Consumed by every node agent image.
FROM node:22-alpine AS build-ts-sdk
WORKDIR /sdk
COPY sdks/typescript/package.json sdks/typescript/tsconfig.json ./
COPY sdks/typescript/src ./src
RUN npm install --no-audit --no-fund && npm run build

# Weather-monitor Node.js/Flue build
FROM node:22-alpine AS build-weather-monitor-node
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/weather-monitor/package*.json ./
RUN npm ci
COPY agents/weather-monitor/.flue ./.flue
COPY agents/weather-monitor/tsconfig.json ./tsconfig.json
RUN npm run build

# Final weather-monitor image
FROM node:22-slim AS weather-monitor
WORKDIR /app
COPY --from=build-weather-monitor-node /src/agents/app/dist ./dist
COPY --from=build-weather-monitor-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
ENV PORT=8080
EXPOSE 8080
CMD ["node", "dist/server.mjs"]

# Chain-worker Node.js build (deterministic loop — no Flue/LLM)
FROM node:22-alpine AS build-chain-worker-node
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/chain-worker/package*.json ./
RUN npm ci
COPY agents/chain-worker/src ./src
COPY agents/chain-worker/tsconfig.json ./tsconfig.json
RUN npm run build

# Final chain-worker image
FROM node:22-slim AS chain-worker
WORKDIR /app
COPY --from=build-chain-worker-node /src/agents/app/dist ./dist
COPY --from=build-chain-worker-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
CMD ["node", "dist/index.js"]

# ── Keycloak (IdP) ─────────────────────────────────────────────────────────
# Custom Keycloak image that runs `kc.sh build` for an optimized server. It is
# the platform's sole IdP: humans log in via Keycloak, and agents mint tokens
# via the custom attestation SPI baked in below. Base pinned by DIGEST (26.4) so
# the build is reproducible and can't drift under a moving tag.
#
# Features enabled at BUILD time (they can't be turned on at runtime on an
# optimized image):
#   - token-exchange — the gated legacy (v1) token-exchange preview, needed by
#     Phase 3's delegation extension. NOTE (verified on this pinned 26.4): the
#     STANDARD token-exchange endpoint (formerly "v2") is now DEFAULT-ON and
#     needs no flag; only the legacy v1 grant is gated behind `token-exchange`.
#   - CIBA needs NO build flag on 26.4 — it is a SUPPORTED feature configured
#     per-realm/client (Phase 4 enables it via Terraform), not a preview flag.
#     (`--features=ciba` is silently ignored by `kc.sh build` here.)
# The Spawnly SPI provider JARs (Phase 1: the attestation client authenticator
# + SPIFFE verifier behind the ATTESTOR seam) are built here and baked in below.
# JARs ONLY — NO secrets in any layer: admin creds, client secrets and user
# passwords all arrive at runtime via Secrets / Terraform variables.

# ── build-keycloak-providers: compile the custom SPI JAR ───────────────────
# Maven build of keycloak/providers (Java 17). Dependencies are `provided`
# (Keycloak ships them), so the output is a thin providers-only JAR with no
# shaded deps. Tests run in CI, not here (kept the image build fast + offline-
# capable of downloading only compile deps).
FROM maven:3.9-eclipse-temurin-17 AS build-keycloak-providers
WORKDIR /src
COPY keycloak/providers/pom.xml .
# Warm the dependency cache in its own layer so source-only edits don't re-fetch.
RUN mvn -q -e -B dependency:go-offline || true
COPY keycloak/providers/src ./src
RUN mvn -q -B -DskipTests package

FROM quay.io/keycloak/keycloak:26.4@sha256:9409c59bdfb65dbffa20b11e6f18b8abb9281d480c7ca402f51ed3d5977e6007 AS build-keycloak
# The realm issuer is fixed to the in-cluster URL by the manifest's hostname
# env at runtime; the build step only freezes the DB vendor, feature set and the
# build-time server options. `--health-enabled=true` MUST be baked here: on an
# optimized image build-time options can't be set at runtime (a runtime
# KC_HEALTH_ENABLED is IGNORED), and the manifest's readiness probe hits
# /health/ready on the management port.
# The provider JAR MUST be copied into /opt/keycloak/providers/ BEFORE
# `kc.sh build` so the SPI is compiled into the optimized image (a JAR added
# after an optimized build is ignored).
# The DB vendor is a BUILD-TIME option on an optimized image: `kc.sh build`
# below freezes it into the server, so a runtime KC_DB is IGNORED — hence the
# ARG (baked via ENV BEFORE `kc.sh build`). kind builds keep the dev-file
# default (file-backed H2 under /opt/keycloak/data — ephemeral only because
# the pod filesystem is); the AWS image build (spawnly-infra's push-images.sh,
# wired in the aws-persistence Phase 5 infra change) passes
# `--build-arg KC_DB=postgres` so the optimized image talks to RDS. The
# postgres JDBC driver already ships in the base image, so switching vendor
# needs no extra build flags.
ARG KC_DB=dev-file
ENV KC_DB=${KC_DB}
COPY --from=build-keycloak-providers /src/target/spawnly-keycloak-providers.jar /opt/keycloak/providers/
# --spi-ciba-auth-channel-provider-default: select the custom Spawnly CIBA
# authentication-channel provider as the realm's decoupled channel (Phase 4).
# This is a BUILD-TIME SPI option on an optimized image (a runtime env is
# rejected with "build time options ... will NOT be used"), so it is baked here
# — two channel factories now exist (built-in ciba-http-auth-channel + ours) and
# the default must be pinned. Ours drives consent through the REGISTRY
# (dashboard/mobile) rather than an HTTP channel or Keycloak's built-in consent
# screen.
# --spi-ciba-login-user-resolver-provider-default: select the custom Spawnly
# CIBA login-user resolver, which accepts login_hint="user:<id>" (the form the
# sidecar sends) as well as a bare "<id>", matching the C# CibaUserValidator.
# Also a build-time SPI option on an optimized image.
# NOTE (Keycloak 26.4 SPI option syntax): the separator between the SPI name and
# the option is a DOUBLE dash (`--spi-<spi>--<option>`). The single-dash legacy
# form (`--spi-<spi>-provider-default`) is NOT treated as a build-time option on
# 26.4 (it logs "using the legacy format … not being treated as build time") and
# the default selection silently doesn't take effect.
RUN /opt/keycloak/bin/kc.sh build \
    --features=token-exchange \
    --health-enabled=true \
    --spi-ciba-auth-channel--provider-default=spawnly-ciba-channel \
    --spi-ciba-login-user-resolver--provider-default=spawnly-ciba-user-resolver

FROM quay.io/keycloak/keycloak:26.4@sha256:9409c59bdfb65dbffa20b11e6f18b8abb9281d480c7ca402f51ed3d5977e6007 AS keycloak
COPY --from=build-keycloak /opt/keycloak/ /opt/keycloak/
# Custom `spawnly` login theme (CSS-only reskin of keycloak.v2). Directory
# themes under /opt/keycloak/themes/ are loaded at RUNTIME on the optimized
# image, so this needs no `kc.sh build` change — copy it straight in.
COPY keycloak/themes/spawnly /opt/keycloak/themes/spawnly
# Start the pre-built (optimized) server; the manifest supplies KC_HOSTNAME /
# KC_HTTP_ENABLED / admin bootstrap creds so no secret is baked here.
ENTRYPOINT ["/opt/keycloak/bin/kc.sh"]
CMD ["start", "--optimized"]

# travel-tools MCP server — a resource server (validates Spawnly tokens), NOT an
# agent, so it does not depend on the @spawnly/sdk. Dev deps pruned after build.
FROM node:22-alpine AS build-travel-tools-node
WORKDIR /src/mcp/travel-tools
COPY mcp/travel-tools/package.json mcp/travel-tools/package-lock.json ./
RUN npm ci
COPY mcp/travel-tools/tsconfig.json ./tsconfig.json
COPY mcp/travel-tools/src ./src
RUN npm run build && npm prune --omit=dev

FROM node:22-slim AS travel-tools
WORKDIR /app
ENV NODE_ENV=production
COPY --from=build-travel-tools-node /src/mcp/travel-tools/package.json ./package.json
COPY --from=build-travel-tools-node /src/mcp/travel-tools/node_modules ./node_modules
COPY --from=build-travel-tools-node /src/mcp/travel-tools/dist ./dist
# Run as the non-root `node` user (uid 1000) baked into the node images.
USER node
EXPOSE 8080
CMD ["node", "dist/index.js"]

# travel-specialist — shared MCP-client agent image; the flight-search /
# hotel-search / fx-converter agent TYPES all run this image with different
# MCP_TOOL/MCP_SCOPE env (set by their templates). Mirrors the chain-worker build.
FROM node:22-alpine AS build-travel-specialist-node
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/travel-specialist/package*.json ./
RUN npm ci
COPY agents/travel-specialist/src ./src
COPY agents/travel-specialist/tsconfig.json ./tsconfig.json
RUN npm run build

FROM node:22-slim AS travel-specialist
WORKDIR /app
COPY --from=build-travel-specialist-node /src/agents/app/dist ./dist
COPY --from=build-travel-specialist-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
EXPOSE 8080
CMD ["node", "dist/index.js"]

# travel-planner — deterministic (no-LLM) orchestrator that fans out to the
# consent-gated specialists. A plain Node TS build (no Flue runtime).
FROM node:22-alpine AS build-travel-planner-node
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/travel-planner/package*.json ./
RUN npm ci
COPY agents/travel-planner/src ./src
COPY agents/travel-planner/tsconfig.json ./tsconfig.json
RUN npm run build

FROM node:22-slim AS travel-planner
WORKDIR /app
COPY --from=build-travel-planner-node /src/agents/app/dist ./dist
COPY --from=build-travel-planner-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
CMD ["node", "dist/index.js"]
