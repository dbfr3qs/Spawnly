---
title: "Chatting with a Long-Lived Agent"
description: How the dashboard chat interface works, what lifecycle:long-lived turns on, and the /agents/chat/:sessionId contract an agent must serve.
---

# Chatting with a Long-Lived Agent

> **Prerequisite:** [Anatomy of an Agent](00-anatomy.md), and ideally
> [Scenario 2 — Loop-Until-Stopped](02-loop-until-stopped.md) (chat is a
> long-lived concern).
>
> **Reference implementation:** [`agents/weather-monitor`](../../agents/weather-monitor),
> a long-lived agent you can chat with from the dashboard to check the weather.
> Its chat handler is [`.flue/agents/chat.ts`](../../agents/weather-monitor/.flue/agents/chat.ts).

The dashboard can hold a back-and-forth conversation with an agent. This page
explains how that works **from a configuration point of view** — what you set,
what the platform does for you, and what your agent has to implement.

## The only knob is `lifecycle: long-lived`

There is no separate "chat" flag. The single switch is the `lifecycle` field in
your template's `runtimeSpec`:

```json
{
  "runtimeSpec": {
    "lifecycle": "long-lived"
  }
}
```

Setting it to `long-lived` turns on three independent things across three
components:

| What | Where | Component |
|------|-------|-----------|
| The 💬 **Chat** button appears on the agent card | gated on `lifecycle === 'long-lived'` | [`cmd/dashboard/static/index.html`](../../cmd/dashboard/static/index.html) |
| A **`{id}-svc` Service** (port 8080) is created — long-lived only | `if aw.Spec.Lifecycle == "long-lived"` | [`internal/operator/reconciler.go`](../../internal/operator/reconciler.go) |
| Chat messages are **routed** to that Service | `POST /v1/agents/{id}/message` → the agent | [`cmd/orchestrator/main.go`](../../cmd/orchestrator/main.go) |

A short-lived agent has none of these: no button, no Service, no route.

## The round-trip

![Chat message round-trip](../chat-flow.svg)

A message travels:

```
Dashboard  →  POST /api/agents/{id}/message          (dashboard proxy)
           →  POST /v1/agents/{id}/message            (orchestrator)
           →  POST http://{id}-svc:8080/agents/chat/{sessionId}   (your agent)
```

The dashboard sends `{ message, sessionId }` and uses the agent id as the
`sessionId`, so each agent card has its own conversation.

## What your agent must implement

The platform routes the message, but **serving the endpoint is the agent's
job.** The contract is:

- **Route:** `POST /agents/chat/:sessionId` on port **8080**.
- **Request body:** `{ "message": "...", "sessionId": "..." }`.
- **Response:** JSON the dashboard can read — `{ "response": "..." }`, or the
  Flue webhook envelope `{ "result": { "response": "..." } }` (the dashboard
  unwraps either shape).

If a long-lived agent does **not** serve this route, the Chat button still
appears (see [Known gap](#known-gap)), but sending a message fails with
`agent unreachable` (502) or a 404 from the agent.

## Flue agents implement chat for free

For TypeScript agents, you do **not** hand-write the HTTP server. `flue build`
generates `dist/server.mjs`, which automatically serves `/agents/chat/:sessionId`
for any **webhook agent**. "Implementing chat" reduces to writing a handler that
returns `{ response }`:

```ts
// .flue/agents/chat.ts
export const triggers = { webhook: true };

interface ChatPayload { message: string; sessionId?: string; }

export default async function ({ payload }: { payload: ChatPayload }) {
  // ...call the LLM, run tools, etc...
  return { response: "…", agentId, timestamp: new Date().toISOString() };
}
```

The framework provides the server, the route, and the `:sessionId` path param.
The reference [`weather-monitor` handler](../../agents/weather-monitor/.flue/agents/chat.ts)
builds on this: it keeps **one LLM session per `sessionId`** (so follow-up
questions retain context), exposes a `get_weather` tool, and returns the model's
reply as `{ response }`.

## Non-Flue agents

A non-Flue agent must implement `POST /agents/chat/:sessionId` itself. Note that
the Go worker ([`cmd/agent`](../../cmd/agent)) is short-lived and serves no chat
route, and the A2A agents (e.g. `currency-converter`) serve JSON-RPC at `/`
rather than the chat path. So **chat currently works against the Flue agents
only**; any other long-lived agent would need to add the endpoint.

## What the dashboard does

- Uses the agent id as `sessionId`, giving each agent its own conversation
  thread held in the browser.
- Shows a **typing indicator** and disables the input while the agent is
  working, then renders the reply.
- Unwraps both `{ response }` and `{ result: { response } }`.

## Try it

From the dashboard: spawn a long-lived agent (e.g. `weather-monitor`), expand
its card, click **💬 Chat**, and ask a question.

From the CLI (orchestrator port-forwarded to `:8080`):

```bash
curl -X POST http://localhost:8080/v1/agents/<workloadName>/message \
  -H 'Content-Type: application/json' \
  -d '{"message":"What is the weather in Tokyo?","sessionId":"<workloadName>"}'
# → {"result":{"response":"…"},"_meta":{...}}
```

## Known gap

The Chat button is gated **purely on `lifecycle`** — the platform never checks
that the agent actually serves `/agents/chat/:sessionId`. A long-lived agent
that isn't chat-capable will still show the button, and messages to it fail.
Capability is currently conflated with lifecycle; an explicit `supportsChat`
template flag that gates the button independently would close this gap (not yet
implemented).
