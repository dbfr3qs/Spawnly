# @spawnly/sdk (TypeScript)

A per-language adapter over the platform's neutral HTTP contract (the local
sidecar `/token` endpoint and the registry events endpoint). See `src/` for the
`TokenClient`, event helpers, and Flue instrumentation.

## Consumer-hosted (remote) agents — parity note

Remote (consumer-hosted) agents are supported at the **platform** level for any
language: an org member registers the agent with their user token, then runs the
`agent-sidecar` (`AGENT_PLACEMENT=remote`, `ATTESTOR=aws-sts`, `AGENT_ID=<id>`)
alongside the agent on their own compute, authenticating with the org's anchored
AWS STS identity. The agent's process only ever talks to the local sidecar's
`/token` endpoint, which is **unchanged** — so an existing TS agent works as a
remote agent with no SDK changes.

A typed TypeScript `registerRemoteAgent(...)` convenience (the parity of the Go
SDK's `RegisterRemoteAgent`) is **not yet ported** — for now, register from
TypeScript with a plain `fetch`:

```ts
const res = await fetch(`${orchestratorURL}/v1/orgs/${orgId}/agents`, {
  method: 'POST',
  headers: {
    authorization: `Bearer ${memberToken}`,
    'content-type': 'application/json',
  },
  body: JSON.stringify({ agentType }),
});
const { agentId } = await res.json(); // "remote-<shortID>"
```

Then run the sidecar + agent with the env described in the Go SDK README
(`sdks/go/README.md`). Full walkthrough and rationale live there.
