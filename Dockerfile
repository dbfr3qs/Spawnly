FROM golang:1.25-alpine AS builder
WORKDIR /app
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

FROM builder AS build-agent
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/agent ./cmd/agent

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

FROM gcr.io/distroless/static-debian12 AS agent
COPY --from=build-agent /bin/agent /
ENTRYPOINT ["/agent"]

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
FROM node:22-alpine AS build-sdk
WORKDIR /sdk
COPY agents/sdk/package.json agents/sdk/tsconfig.json ./
COPY agents/sdk/src ./src
RUN npm install --no-audit --no-fund && npm run build

# Weather-monitor Node.js/Flue build
FROM node:22-alpine AS build-weather-monitor-node
WORKDIR /app
COPY agents/sdk/ /sdk/
COPY agents/weather-monitor/package*.json ./
RUN npm ci
COPY agents/weather-monitor/.flue ./.flue
COPY agents/weather-monitor/tsconfig.json ./tsconfig.json
RUN npm run build

# Final weather-monitor image
FROM node:22-slim AS weather-monitor
WORKDIR /app
COPY --from=build-weather-monitor-node /app/dist ./dist
COPY --from=build-weather-monitor-node /app/node_modules ./node_modules
COPY agents/sdk/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
ENV PORT=8080
EXPOSE 8080
CMD ["node", "dist/server.mjs"]

# Child-agent Node.js/Flue build
FROM node:22-alpine AS build-child-agent-node
WORKDIR /app
COPY agents/sdk/ /sdk/
COPY agents/child-agent/package*.json ./
RUN npm ci
COPY agents/child-agent/src ./src
COPY agents/child-agent/tsconfig.json ./tsconfig.json
RUN npm run build

# Final child-agent image
FROM node:22-slim AS child-agent
WORKDIR /app
COPY --from=build-child-agent-node /app/dist ./dist
COPY --from=build-child-agent-node /app/node_modules ./node_modules
COPY agents/sdk/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
EXPOSE 8080
CMD ["node", "dist/index.js"]

# Parent-agent Node.js/Flue build
FROM node:22-alpine AS build-parent-agent-node
WORKDIR /app
COPY agents/sdk/ /sdk/
COPY agents/parent-agent/package*.json ./
RUN npm ci
COPY agents/parent-agent/src ./src
COPY agents/parent-agent/tsconfig.json ./tsconfig.json
RUN npm run build

# Final parent-agent image
FROM node:22-slim AS parent-agent
WORKDIR /app
COPY --from=build-parent-agent-node /app/dist ./dist
COPY --from=build-parent-agent-node /app/node_modules ./node_modules
COPY agents/sdk/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
CMD ["node", "dist/index.js"]

# Currency-converter Node.js/Flue build
FROM node:22-alpine AS build-currency-converter-node
WORKDIR /app
COPY agents/sdk/ /sdk/
COPY agents/currency-converter/package*.json ./
RUN npm ci
COPY agents/currency-converter/src ./src
COPY agents/currency-converter/tsconfig.json ./tsconfig.json
RUN npm run build

# Final currency-converter image
FROM node:22-slim AS currency-converter
WORKDIR /app
COPY --from=build-currency-converter-node /app/dist ./dist
COPY --from=build-currency-converter-node /app/node_modules ./node_modules
COPY agents/sdk/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
EXPOSE 8080
CMD ["node", "dist/index.js"]

# Trip-planner Node.js/Flue build
FROM node:22-alpine AS build-trip-planner-node
WORKDIR /app
COPY agents/sdk/ /sdk/
COPY agents/trip-planner/package*.json ./
RUN npm ci
COPY agents/trip-planner/src ./src
COPY agents/trip-planner/tsconfig.json ./tsconfig.json
RUN npm run build

# Final trip-planner image
FROM node:22-slim AS trip-planner
WORKDIR /app
COPY --from=build-trip-planner-node /app/dist ./dist
COPY --from=build-trip-planner-node /app/node_modules ./node_modules
COPY agents/sdk/package.json ./node_modules/@spawnly/sdk/package.json
COPY --from=build-sdk /sdk/dist/ ./node_modules/@spawnly/sdk/dist/
CMD ["node", "dist/index.js"]

FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build-identity-server
WORKDIR /src
COPY identityserver/ .
RUN dotnet publish -c Release -o /app/publish

FROM mcr.microsoft.com/dotnet/aspnet:8.0 AS identity-server
WORKDIR /app
COPY --from=build-identity-server /app/publish .
ENTRYPOINT ["dotnet", "IdentityServer.dll"]
