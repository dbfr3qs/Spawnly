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

# go-worker is a separate Go module (github.com/spawnly/go-worker) with a
# replace directive pointing at ../../sdks/go, so it builds in module mode
# from its own checkout rather than the root builder stage.
FROM golang:1.25-alpine AS build-go-worker
WORKDIR /src
COPY sdks/go/ ./sdks/go/
COPY agents/go-worker/ ./agents/go-worker/
WORKDIR /src/agents/go-worker
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/go-worker .

FROM gcr.io/distroless/static-debian12 AS go-worker
COPY --from=build-go-worker /bin/go-worker /
ENTRYPOINT ["/go-worker"]

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

# Child-agent Node.js/Flue build
FROM node:22-alpine AS build-child-agent-node
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/child-agent/package*.json ./
RUN npm ci
COPY agents/child-agent/src ./src
COPY agents/child-agent/tsconfig.json ./tsconfig.json
RUN npm run build

# Final child-agent image
FROM node:22-slim AS child-agent
WORKDIR /app
COPY --from=build-child-agent-node /src/agents/app/dist ./dist
COPY --from=build-child-agent-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
EXPOSE 8080
CMD ["node", "dist/index.js"]

# Parent-agent Node.js/Flue build
FROM node:22-alpine AS build-parent-agent-node
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/parent-agent/package*.json ./
RUN npm ci
COPY agents/parent-agent/src ./src
COPY agents/parent-agent/tsconfig.json ./tsconfig.json
RUN npm run build

# Final parent-agent image
FROM node:22-slim AS parent-agent
WORKDIR /app
COPY --from=build-parent-agent-node /src/agents/app/dist ./dist
COPY --from=build-parent-agent-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
CMD ["node", "dist/index.js"]

# Currency-converter Node.js/Flue build
FROM node:22-alpine AS build-currency-converter-node
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/currency-converter/package*.json ./
RUN npm ci
COPY agents/currency-converter/src ./src
COPY agents/currency-converter/tsconfig.json ./tsconfig.json
RUN npm run build

# Final currency-converter image
FROM node:22-slim AS currency-converter
WORKDIR /app
COPY --from=build-currency-converter-node /src/agents/app/dist ./dist
COPY --from=build-currency-converter-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
EXPOSE 8080
CMD ["node", "dist/index.js"]

# Trip-planner Node.js/Flue build
FROM node:22-alpine AS build-trip-planner-node
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/trip-planner/package*.json ./
RUN npm ci
COPY agents/trip-planner/src ./src
COPY agents/trip-planner/tsconfig.json ./tsconfig.json
RUN npm run build

# Final trip-planner image
FROM node:22-slim AS trip-planner
WORKDIR /app
COPY --from=build-trip-planner-node /src/agents/app/dist ./dist
COPY --from=build-trip-planner-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
CMD ["node", "dist/index.js"]

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

# Pi-worker Node build (Pi coding harness — @earendil-works/pi-coding-agent).
# Built on node:22-slim (Debian, glibc) — same base as the runtime image — so the
# installed node_modules match the runtime libc (Pi's dep tree is large; this
# avoids any musl/glibc mismatch from the alpine→slim copy the other agents use).
# Build toolchain (python3/make/g++) covers any node-gyp dep during npm ci.
FROM node:22-slim AS build-pi-worker-node
RUN apt-get update && apt-get install -y --no-install-recommends python3 make g++ git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
# WORKDIR mirrors the host layout (agents/<name> two levels below sdks/typescript)
# so the lockfile's file:../../sdks/typescript link resolves identically here.
WORKDIR /src/agents/app
COPY sdks/typescript/ /src/sdks/typescript/
COPY agents/pi-worker/package*.json ./
RUN npm ci
COPY agents/pi-worker/src ./src
COPY agents/pi-worker/tsconfig.json ./tsconfig.json
RUN npm run build

# Final pi-worker image. node:22-slim (not distroless) + git because Pi is a
# CODING agent: its built-in tools shell out (bash, already in slim) and edit
# files in a scratch workspace (/work).
FROM node:22-slim AS pi-worker
RUN apt-get update && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build-pi-worker-node /src/agents/app/dist ./dist
COPY --from=build-pi-worker-node /src/agents/app/node_modules ./node_modules
COPY sdks/typescript/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-ts-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
RUN mkdir -p /work
ENV HOME=/root PI_WORKDIR=/work
EXPOSE 8080
CMD ["node", "dist/index.js"]

FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build-identity-server
WORKDIR /src
COPY identityserver/ .
RUN dotnet publish -c Release -o /app/publish

FROM mcr.microsoft.com/dotnet/aspnet:8.0 AS identity-server
WORKDIR /app
COPY --from=build-identity-server /app/publish .
ENTRYPOINT ["dotnet", "IdentityServer.dll"]

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
# MCP_TOOL/MCP_SCOPE env (set by their templates). Mirrors the child-agent build.
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
