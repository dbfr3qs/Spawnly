# Spawnly Go SDK

A per-language adapter over the platform's **neutral HTTP contract**. It depends
only on the Go standard library and never imports a platform package
(dependencies point one way, from this adapter toward the contract — see
`doc.go`).

- `TokenClient` — mint/exchange tokens via the local sidecar (`localhost:8089`).
- `AuthenticatedClient` — HTTP calls with an auto-attached bearer + tenant header.
- `PostEvent` — post a lifecycle/work event to the registry.
- `RegisterRemoteAgent` — register a **consumer-hosted** agent with an org.

## Consumer-hosted (remote) agents

A remote agent runs on **your own compute** (not Spawnly's cluster) and
authenticates to Spawnly with your organization's **anchored AWS STS identity**.
There is no operator on your side — you run the sidecar + your agent yourself.

Prerequisites (one-time, by an org admin):

1. The org has an **active attestation anchor** — an org admin registers an STS
   anchor (`awsAccountId` + `roleArnPattern`) in the dashboard and activates it
   by presenting a signed `GetCallerIdentity` from that account (proof of
   control). Until the anchor is active, remote registration is refused.

### 1. Register the agent (org member, user token)

```go
agentID, err := spawnly.RegisterRemoteAgent(ctx, spawnly.RemoteRegistration{
    OrchestratorURL: "https://spawnly.run",
    OrgID:           "acme",
    MemberToken:     memberAccessToken, // the registering member's delegated token
    AgentType:       "weather-monitor", // a template visible in the org
})
// agentID == "remote-<shortID>" — the id whose record is now preregistered
// (Placement=remote, OrgID=acme, owned by the member's sub).
```

The registering member **owns** the agent, so ownership/consent/revoke semantics
match a platform-hosted spawn.

### 2. Run the sidecar + your agent (your compute)

Run the `agent-sidecar` alongside your agent process with:

| Env | Value |
|-----|-------|
| `AGENT_PLACEMENT` | `remote` |
| `AGENT_ID` | the `agentID` from step 1 |
| `AGENT_TYPE` | the registered agent type (OAuth client_id) |
| `ATTESTOR` | `aws-sts` |
| `REGISTRY_URL` | the public registry base URL |
| `IS_TOKEN_URL` | the public Keycloak token endpoint |
| `AWS_ROLE_SESSION_NAME` | the `agentID` (the STS session name **is** the agentId) |

Provide the sidecar with **ambient AWS credentials** for a role matching the
org's anchor (`roleArnPattern`), assumed with `--role-session-name <agentID>`.
Spawnly never sees these credentials — the sidecar presigns a
`GetCallerIdentity` request and forwards only that presigned presentation.

The sidecar self-registers (flipping the record `active`) and serves tokens on
`localhost:8089`. Your agent mints exactly as a platform-hosted one does:

```go
tc := spawnly.NewTokenClient()
token, err := tc.GetToken(ctx, "openid")
```

The agent now appears in the registry/dashboard with full lifecycle parity:
**list, events, and revoke all work** — revocation is enforced in SpiceDB, which
is runtime-agnostic, so a dashboard revoke severs the remote agent's next mint.

### Isolation

A given STS identity can only ever authenticate an agent whose registry record
belongs to that identity's org anchor. Presenting org A's identity for an agent
recorded under org B (or an unanchored org) is refused end-to-end.
