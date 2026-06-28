---
name: spawnly-platform
description: >-
  Expert knowledge of the Spawnly agent platform — how agents get their SPIFFE
  identity, how tokens are minted, how spawn consent (CIBA), delegation,
  token-exchange, revoke/resume cascades, and tenancy work, plus its Make/API
  surface and operational gotchas. Use whenever the user asks how Spawnly works,
  why an agent behaved a certain way (e.g. got a 403), or how to operate the
  cluster — including plain-English questions, not just /spawnly commands.
---

# Spawnly platform

Spawnly is a Kubernetes-native multi-agent platform where every agent runs as a
pod with a sidecar, gets a cryptographic workload identity from SPIRE, and mints
short-lived OAuth access tokens to call protected APIs. Authority is delegated,
revocable in real time, and (optionally) gated on human consent.

## Mental model

- **Control plane (Go):** `registry` (agent records, templates, consent store,
  lineage), `orchestrator` (spawn API, dashboard backend), `operator` (the
  AgentWorkload CRD reconciler that builds pods), `dashboard` (browser UI).
- **Identity:** SPIRE issues each pod a **JWT-SVID** via the SPIFFE Workload API
  (no secrets on disk). That SVID is the agent's proof of who it is.
- **Tokens:** IdentityServer (Duende) verifies the SVID and mints an access
  token whose `sub` is the human user and whose `act` is the agent.
- **Authorization:** SpiceDB holds the relationships; resource servers check it
  on every call, so revocation is real-time.
- **Sidecar:** runs next to the agent, owns identity + token minting + the
  consent flow, and serves `/token` on localhost. **Agent code never touches
  identity** — it just asks the sidecar for a token.

## How to answer "how does X work?" questions

**Read the source of truth before you answer. Cite `file:line`. Do not recite
generic SPIFFE/OAuth from memory** — this platform has specific choices, and the
code is the truth. After explaining, offer to **show it live** on the cluster.

### Concept → source of truth

| Concept | Read these first |
|---|---|
| Agent identity / attestation | `internal/operator/reconciler.go` (buildPod: pod labels + the `csi.spiffe.io` workload-API mount + `SPIFFE_ENDPOINT_SOCKET`), `deploy/spire/clusterspiffeid.yaml` (the SPIFFE ID templates), `cmd/agent-sidecar/main.go` (`fetchJWT` → `workloadapi.FetchJWTSVID`) |
| Token minting (client_credentials) | `identityserver/AgentRegistryValidator.cs` (derives agentId from the SVID, sets `sub=user:<id>` + `act`), `identityserver/SpireClientSecretValidator.cs` (verifies the SVID against SPIRE's JWKS), `docs/internals/token-minting.md` |
| Spawn consent (CIBA) | `docs/internals/spawn-consent.md`, `identityserver/CibaRequestValidator.cs` (binds the request to a registry-derived spawn edge), `identityserver/CibaConsentNotificationService.cs` (auto-approve vs. prompt), `identityserver/CibaCompletionService.cs` (records the grant + releases pending requests for the edge), `cmd/agent-sidecar/ciba.go` (the sidecar driver) |
| Mobile consent | `docs/internals/mobile-ciba.md`, `cmd/mobile-gateway/` + `internal/mobilegateway/` (user-token consent proxy to the orchestrator, device registry, `/internal/notify` push fan-out on :8081, `/me/stream` SSE on :8080), `mobile/` (Expo app), the public PKCE `mobile` client in `identityserver/Config.cs`. The phone answers the SAME registry-owned consent request as the dashboard — `NOTIFIER=dev` streams it locally (no cloud creds), `fcmapns` pushes on AWS |
| Delegation / token-exchange | `identityserver/TokenExchangeGrantValidator.cs`, the registry spawn-policy handler in `cmd/registry/main.go` (`allowedChildTypes`, `maxDepth`, `childPolicies`) |
| Revoke / resume cascade | `cmd/registry/main.go` (`subtree()`, `revokeNode`, `resumeNode`) — drops/restores SpiceDB authority across an agent's whole descendant subtree |
| Agent ownership scoping | A user only sees/acts on agents whose record `UserID` is theirs. Enforced via `?userId=` (the asserted session user): `agentOwnedBy` in `cmd/registry/main.go` gates revoke/resume/dismiss/subtree; the orchestrator (`cmd/orchestrator/main.go`, `ownsAgent`) gates the list + events/logs/message; the dashboard injects `auth.user(r)` and PathEscapes the id (`agentOpTarget`) so a crafted id can't smuggle a different userId. Deny = 404 (no existence oracle) |
| Authenticated spawn | `cmd/orchestrator/main.go` `POST /spawn` derives identity, never trusts the body: agent path validates an `aud=orchestrator` JWT (`internal/tokenvalidator`) → `userId` from `sub`, `parentId` from `act`; dashboard path uses the `X-Control-Plane-Token` header. Audience/scope from `identityserver/Config.cs` (`orchestrator` ApiResource + `orchestrator:spawn` scope); SDK `spawn()` mints the token |
| Tenancy / global scope | `deploy/spire/clusterspiffeid.yaml` (tenant vs. global ID templates), the registry register handler (tenant tuples skipped for global agents) |
| Lifecycle events | `cmd/agent-sidecar/main.go` + `internal/events` — the neutral event taxonomy (`svid_fetched`, `token_issued`, `work_ok`, `work_denied`, `consent_requested`, `consent_granted`, `consent_denied`) the dashboard and tests read |

### Showing it live (escalate explanation → demonstration)

- Decode the SVID an agent holds: exec into a pod's sidecar and inspect the
  JWT-SVID `sub`, or read the `svid_fetched`/`token_issued` events for an agent.
- `kubectl get clusterspiffeid -o yaml` to show the ID templates SPIRE applies.
- `kubectl get pod <agent>-pod -o jsonpath='{.spec.volumes}'` to show the
  `csi.spiffe.io` mount that delivers identity.
- The agent's event timeline: `GET /v1/agents/<id>/events` (via the orchestrator
  or registry) traces identity → token → work end to end.

## Operating surface

**Make targets** (run from repo root): `bootstrap` (Kind + SPIRE + images +
deploy + seed), `demo`, `dash` (port-forward dashboard), `e2e`, `reseed`
(re-seed templates — the registry store is in-memory and resets on restart),
`redeploy-<svc>` (rebuild+load+roll a Go service: registry, orchestrator,
operator, dashboard, sample-api, identity-server), `reload-<agent>` (rebuild+load
a TypeScript agent image), `reload-sidecar`, `logs-<svc>`, `kind-down`.

**Ports:** dashboard `localhost:8090`, orchestrator `localhost:8080` (both via
`make demo` or `kubectl port-forward`).

**Spawn an agent:** `POST localhost:8080/spawn` → `{"workloadName"}`.
Authenticated: an **agent** caller sends a `Bearer` token audienced for the
orchestrator (the SDK `spawn()` mints it) and the orchestrator derives
`userId`/`parentId`/`tenantId` from the token+parent record — the body only
carries `agentType` (+`task`). A **dashboard/human** caller sends no bearer but
an `X-Control-Plane-Token` header and the body's `userId` (the dashboard injects
the session user) for a top-level spawn (no `parentId`). Omit `tenantId`
for a global agent. Agent types: `chain-worker`, `weather-monitor`,
`travel-planner`, and the three travel specialists `flight-search`,
`hotel-search`, `fx-converter` (each runs the shared `travel-specialist` image).

**Key read APIs** (orchestrator on :8080, or registry):
`GET /v1/agents`, `GET /v1/agents/{id}/events`, `GET /v1/templates`,
`GET /v1/consents`. Registry adds `GET /v1/agents/{id}/chain`,
`GET /v1/consents/check`, `GET /v1/spawn-policy`.

**Lifecycle actions:** `POST /v1/agents/{id}/revoke|resume|dismiss`,
`DELETE /v1/agents/{id}` (kill — **cascades** to the whole descendant subtree,
like revoke). All are ownership-scoped: callers pass `?userId=` and the registry
404s an agent the user doesn't own. The dashboard injects the session user.

## Operational gotchas (for diagnosis)

- **In-memory registry** resets templates on restart → `make reseed`.
- **Stuck `spire-spiffe-oidc-discovery-provider`** pod → registry crashloops on
  its JWKS fetch. Fix: force-delete the stuck pod, restart registry, reseed.
- **Stale images:** on a pre-existing cluster the `:latest` manifests are
  no-ops, so a re-bootstrap keeps old code unless deployments are rolled —
  `bootstrap.sh` now `rollout restart`s them; otherwise use `redeploy-<svc>`.
- **Never bake `identityserver/keys/`** into the image (local Duende dev signing
  keys; in-cluster the pod can't unprotect them) — see `.dockerignore`.
- **DELETE doesn't cascade**: killing a chain root orphans its children; tear
  down subtrees explicitly or use revoke for authority.
- **Consent-gated child types** need a client entry in `identityserver/Config.cs`
  with the CIBA grant + `openid` scope, or token minting 400s.

When unsure, prefer reading the file and the live cluster over guessing.
