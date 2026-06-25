---
title: "Scenario 3 — Parent → Child: Consent-Gated Fan-Out"
description: Agent-to-agent orchestration over A2A — a planner fans out to least-privilege specialists, each gated by its own user consent and scoped to exactly one tool — plus the token-exchange delegation path.
---

# Scenario 3 — Parent → Child: Consent-Gated Fan-Out

> **Prerequisite:** [Anatomy of an Agent](00-anatomy.md). This scenario builds on
> both earlier ones — the parent does a job and exits (Scenario 1), each child is
> a long-lived A2A server (Scenario 2 lifecycle).
>
> **Reference implementations:** [`agents/travel-planner`](../../agents/travel-planner)
> (the orchestrator) and [`agents/travel-specialist`](../../agents/travel-specialist)
> (the shared image behind the `flight-search`, `hotel-search`, and `fx-converter`
> types), with the [`travel-tools`](../../mcp/travel-tools) MCP server as the
> protected resource. Templates live beside each agent as `template.json` (seeded
> by [`scripts/seed.sh`](../../scripts/seed.sh)).

## The personalities

The **Travel Planner** (parent) spins up and needs three narrow sub-tasks done —
search flights, search hotels, convert a budget between currencies. Rather than
acquire all three capabilities itself, it **spawns three specialists**, hands
each one task over A2A, collects the real results, assembles an itinerary, tears
the children down, and exits.

Each **specialist** (child) is a long-lived A2A server and a **least-privilege
MCP client**: it holds a Spawnly token scoped to **exactly one** travel-tools
scope (`flights:read`, `hotels:read`, or `fx:read`) and can call **exactly one**
MCP tool with it. The provider API keys (Duffel, LiteAPI, Frankfurter) live in
the MCP server, never in the agent — the agent only ever holds a scope-limited
token.

Two properties make this the interesting case:

1. **Each specialist is a *different* spawn edge**, so the user gets **three
   independent consent prompts** — one per capability — that do not collapse into
   one. Consent to flight search is not consent to currency conversion.
2. **Attenuation is by least privilege, enforced at the resource server.** Each
   specialist's IdentityServer client allows only its single scope, and the
   travel-tools MCP server rejects any tool call whose token lacks that tool's
   scope. The planner grants the children **no** authority of its own
   (`grantableScopes: []`) — each child's authority is its own, minimal, and
   gated by consent.

This is the shape for any "lead agent that fans work out to specialists" where
each capability should be **independently authorized**: research lead →
researchers, planner → tool-specialists, orchestrator → workers.

## The two halves

### Parent — an orchestrator that fans out and exits

The parent is a deterministic (no-LLM) job-and-exit agent (Scenario 1) whose
"job" is to drive three children concurrently. The full implementation is
[`agents/travel-planner/src/index.ts`](../../agents/travel-planner/src/index.ts);
each specialist run is a four-step cycle:

| Step | Does |
|------|------|
| `spawnSpecialist` | `POST /spawn` on the orchestrator with the specialist's `agentType` and `parentId: AGENT_ID`. Returns the child's id. |
| `waitReady` | Polls `http://<childId>-svc:8080/.well-known/agent.json` until the child's A2A server answers. |
| `callSpecialist` | Opens an A2A client to `http://<childId>-svc:8080` and `sendMessage(...)`, passing the tool args in `metadata.params`. **This is when the child mints its scoped token — and, because the edge is consent-gated, when the user's consent prompt appears.** Blocks until consent resolves. |
| `killSpecialist` | `DELETE /v1/agents/<childId>` on the orchestrator. |

The three runs fan out concurrently with `Promise.allSettled` — a denied consent
or a failed specialist must not abort the others, and every branch tears its
child down in a `finally`:

```ts
const [flightsR, hotelsR, fxR] = await Promise.allSettled([
  runSpecialist("flight-search", { origin, destination, departureDate, adults: 1, cabin: "economy" }),
  runSpecialist("hotel-search",  { cityName, countryCode, checkIn, checkOut, adults: 1, currency }),
  runSpecialist("fx-converter",  { amount: budget, from: homeCurrency, to: "AUD" }),
]);
// assemble an itinerary from whatever real results came back; a denied branch
// simply contributes nothing.
```

The parent's template is short-lived (it exits after the round-trip) **and**
carries the delegation policy — here, a per-child **consent** gate rather than a
scope grant:

```json
{
  "agentType": "travel-planner",
  "version": "1.0.0",
  "status": "active",
  "requiresTenant": false,
  "meta": {"displayName": "Travel Planner", "description": "Fans out to three consent-gated specialists and returns an itinerary"},
  "runtimeSpec": {"image": "agent-travel-planner:latest", "resources": {"cpuLimits": "1", "memoryLimits": "512Mi"}, "envDefaults": {}},
  "authzTemplate": {"spiceDbRelations": []},
  "delegation": {
    "allowedChildTypes": ["flight-search", "hotel-search", "fx-converter"],
    "grantableScopes": [],
    "maxDepth": 2,
    "childPolicies": {
      "flight-search": {"requireUserConsent": true, "consentTTL": "720h"},
      "hotel-search":  {"requireUserConsent": true, "consentTTL": "720h"},
      "fx-converter":  {"requireUserConsent": true, "consentTTL": "720h"}
    }
  }
}
```

The `delegation` block is the policy gate: the parent may only spawn the listed
child types (deny-by-default), and each `childPolicies` entry sets
`requireUserConsent: true` so each spawn edge is gated by a CIBA consent prompt
keyed on `(user, travel-planner, <childType>)`. `grantableScopes: []` means the
parent flows **no** authority down — the children are not delegated tokens; they
mint their own. See [05 — Defining Policy](05-defining-policy.md#part-2--delegation)
for the full model and enforcement points, and the
[CIBA consent flow](05-defining-policy.md) for how a prompt is approved, stored
for `consentTTL`, and re-prompted on revocation.

### Child — a long-lived least-privilege MCP client

Each specialist is a long-lived agent (so it gets a `<id>-svc` Service) running
an A2A server. All three run the **same image** ([`agents/travel-specialist`](../../agents/travel-specialist));
the type they register as, and the one tool/scope they may use, come from their
template's `envDefaults`. The implementation is
[`agents/travel-specialist/src/index.ts`](../../agents/travel-specialist/src/index.ts);
the essentials:

1. **Publishes an agent card** at `/.well-known/agent.json` (how the planner's
   `waitReady` discovers it).
2. **On each message, reads the tool args** from `metadata.params`.
3. **Mints a scope-limited token** — `tokens.getToken(MCP_SCOPE)`. It requests
   *only* the scope, never an explicit audience: the token's `aud` is derived
   from the scope's `ApiResource` (`travel-tools`), so a `flights:read` token can
   only ever address the travel-tools server.
4. **Calls its one MCP tool** — `client.callTool({ name: MCP_TOOL, arguments })`
   against the travel-tools MCP server, and replies over A2A.

The attenuation is the headline, and it is enforced server-side. A `flight-search`
specialist holding a `flights:read` token can call `search_flights`, but the same
token presented to `convert_currency` (which requires `fx:read`) is rejected by
the MCP server's scope gate. Each specialist's IdentityServer client
([`identityserver/Config.cs`](../../identityserver/Config.cs)) allows only its one
scope, so it cannot even mint a broader token.

The specialist template is long-lived and declares its single scope plus the
tool/scope wiring (here, `flight-search`):

```json
{
  "agentType": "flight-search",
  "version": "1.0.0",
  "status": "active",
  "requiresTenant": false,
  "oauthScopes": ["openid", "flights:read"],
  "meta": {"displayName": "Flight Search", "description": "Least-privilege specialist: searches flights via the travel-tools search_flights tool"},
  "runtimeSpec": {
    "image": "agent-travel-specialist:latest",
    "lifecycle": "long-lived",
    "resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"},
    "envDefaults": {"MCP_URL": "http://travel-tools/mcp", "MCP_TOOL": "search_flights", "MCP_SCOPE": "flights:read"}
  },
  "authzTemplate": {"spiceDbRelations": []}
}
```

`hotel-search` and `fx-converter` are the same template with
`hotels:read`/`search_hotels` and `fx:read`/`convert_currency` respectively.

## The end-to-end flow

```
Travel Planner (parent)                       flight/hotel/fx specialists (children)
  │  spawnSpecialist ×3 ───────────────────────►  3 pods + <id>-svc each (long-lived)
  │  waitReady  ── GET agent.json ─────────────►  A2A servers ready
  │  callSpecialist ── A2A msg + params ───────►  CIBA consent prompt per child
  │       (blocks on consent)                      └─ approve → mint scope-limited token
  │                                                 callTool on travel-tools MCP server
  │  ◄──────────── A2A reply (real result) ─────  reply over A2A
  │  killSpecialist ── DELETE /v1/agents ──────►  pods torn down
  │  assemble itinerary, report + exit (Completed)
```

## Run it (using the seeded `travel-planner`)

The planner spawns the specialists itself — you only spawn the planner, then
approve the three consent prompts.

```bash
make demo   # port-forwards orchestrator :8080 + dashboard :8090

curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"agentType":"travel-planner","userId":"user-1","task":"trip to Sydney"}'
# -> {"workloadName":"travel-planner-xxxxx"}

# Watch the planner and the three specialists it spawns:
kubectl get agentworkloads -w

# Approve the three consent prompts on the dashboard (http://localhost:8090), or
# via the consent API (GET /v1/consent-requests then POST .../approve).

# Planner timeline: parent_started, specialist_spawned ×3, parent_completed (itinerary):
curl -sf http://localhost:8080/v1/agents/travel-planner-xxxxx/events | jq
```

> The travel-tools MCP server calls real upstream providers; populate its keys
> (Duffel / LiteAPI; Frankfurter needs none) for live data, or it returns an
> error the planner records and routes around. The planner itself runs no LLM.

On the dashboard you'll see the planner plus three specialists appear, **three
separate consent prompts** (one per capability), and — once approved — each
specialist's `tool_result` event carrying real provider data, then the planner's
assembled `parent_completed` itinerary.

## Token-exchange delegation: the other attenuation path

The fan-out above attenuates by **least privilege + consent**: each child mints
its *own* minimal token. The platform also supports the complementary model —
**delegation by token-exchange (RFC 8693)**, where a parent hands a child a slice
of *its own* authority, attenuated, and the child acts **on the user's behalf**.

No example agent ships for this path today, but it remains a first-class platform
capability:

- The parent mints a **delegation token** attenuated to a narrower scope —
  `tokens.getToken(scope, { audience: 'delegation' })` — and passes it to the
  child (e.g. in A2A message metadata).
- The child **exchanges** it for a resource token —
  `tokens.exchangeToken({ subjectToken, audience, scope })` (the SDK's RFC 8693
  helper) — and calls the protected API. The exchanged token keeps `sub` = the
  **user** down the whole chain, so a resource server can authorize on the
  originating user.
- IdentityServer enforces the attenuation at the exchange:
  [`TokenExchangeGrantValidator`](../../identityserver/TokenExchangeGrantValidator.cs)
  rejects an exchange that widens scope beyond what the parent's template
  `grantableScopes` permitted. (A delegation-capable client also needs the
  `token-exchange` grant in [`Config.cs`](../../identityserver/Config.cs).)

**Choosing between them:**

| | Consent-gated fan-out (this scenario) | Token-exchange delegation |
|---|---|---|
| Child's authority | its **own** least-privilege scope | a **slice of the parent's**, attenuated |
| Token `sub` | the child | the **user** (carried down the chain) |
| Gate | per-edge **user consent** (CIBA) | `grantableScopes` checked at the exchange |
| Use when | each capability is independently owned + authorized | a resource must authorize on the originating user |

Both rest on the same orchestration scaffolding (spawn → discover via `<id>-svc`
+ agent card → call over A2A → kill) and the same deny-by-default
`allowedChildTypes` spawn gate; they differ only in **where the child's authority
comes from**. See [05 — Defining Policy](05-defining-policy.md) for the
own-authority vs delegated-authority distinction underneath both.

## What this scenario teaches

- Agent-to-agent orchestration: spawn → discover (`<id>-svc` + agent card) →
  call over A2A → kill.
- Delegation policy in the parent's template (`allowedChildTypes`,
  `childPolicies` consent gates, `grantableScopes`, `maxDepth`).
- **Per-capability consent**: distinct spawn edges produce distinct, non-collapsing
  consent prompts.
- **Least-privilege attenuation enforced at the resource server** — a specialist
  can call only the one tool its single scope permits.
- That **delegation has two shapes**: least-privilege own-authority children (the
  fan-out), and token-exchange that carries the user's identity down the chain.
