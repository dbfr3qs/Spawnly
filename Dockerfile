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

FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build-identity-server
WORKDIR /src
COPY identityserver/ .
RUN dotnet publish -c Release -o /app/publish

FROM mcr.microsoft.com/dotnet/aspnet:8.0 AS identity-server
WORKDIR /app
COPY --from=build-identity-server /app/publish .
ENTRYPOINT ["dotnet", "IdentityServer.dll"]

# ── Keycloak (IdP, Phase 0) ────────────────────────────────────────────────
# Custom Keycloak image that runs `kc.sh build` for an optimized server. Brought
# up NEXT TO identity-server on kind: humans log in via Keycloak, agents stay on
# IdentityServer until later phases. Base pinned by DIGEST (26.4) so the build is
# reproducible and can't drift under a moving tag.
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
# No providers yet (Phase 1 bakes the SPI JARs); NO secrets in any layer — admin
# creds, client secrets and user passwords all arrive at runtime via Secrets /
# Terraform variables.
FROM quay.io/keycloak/keycloak:26.4@sha256:9409c59bdfb65dbffa20b11e6f18b8abb9281d480c7ca402f51ed3d5977e6007 AS build-keycloak
# The realm issuer is fixed to the in-cluster URL by the manifest's hostname
# env at runtime; the build step only freezes the DB vendor, feature set and the
# build-time server options. `--health-enabled=true` MUST be baked here: on an
# optimized image build-time options can't be set at runtime (a runtime
# KC_HEALTH_ENABLED is IGNORED), and the manifest's readiness probe hits
# /health/ready on the management port.
ENV KC_DB=dev-file
RUN /opt/keycloak/bin/kc.sh build --features=token-exchange --health-enabled=true

FROM quay.io/keycloak/keycloak:26.4@sha256:9409c59bdfb65dbffa20b11e6f18b8abb9281d480c7ca402f51ed3d5977e6007 AS keycloak
COPY --from=build-keycloak /opt/keycloak/ /opt/keycloak/
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
