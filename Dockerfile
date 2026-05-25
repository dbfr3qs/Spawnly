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

FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build-identity-server
WORKDIR /src
COPY identityserver/ .
RUN dotnet publish -c Release -o /app/publish

FROM mcr.microsoft.com/dotnet/aspnet:8.0 AS identity-server
WORKDIR /app
COPY --from=build-identity-server /app/publish .
ENTRYPOINT ["dotnet", "IdentityServer.dll"]
