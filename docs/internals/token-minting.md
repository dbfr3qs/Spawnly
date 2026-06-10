---
title: How an agent's token is minted
description: When it is not token exchange — how a workload's JWT-SVID becomes a scoped OAuth access token via the client_credentials grant, and how the sub/act identity is stamped onto it.
---

# How an agent's token is minted

When an agent calls a protected API, it presents a short-lived **OAuth 2.0
access token**. Most of the time that token is *not* produced by token exchange —
it is minted with the plain **`client_credentials`** grant, where the workload's
**JWT-SVID authenticates the client**.

The mental model in one line:

> The **SVID** proves *which workload* is asking (it becomes the token's `act`).
> The **Agent Registry** lookup supplies *whose authority* it acts under (it
> becomes the token's `sub = user:<id>`).

This page traces that mint end to end. For the *delegation* path (a parent
handing attenuated authority to a child via RFC 8693 token exchange), see
[Defining Policy → Delegation](/authoring/05-defining-policy#part-2--delegation).

![Minting a client_credentials token from a workload's JWT-SVID](../token-mint.svg)

## 1. The trigger

The agent never talks to IdentityServer directly. It asks its **sidecar** for a
scoped token through the SDK's `TokenClient`:

```ts
const accessToken = await tokens.getToken('sample-api-a:write');
```

That call hits the sidecar's local endpoint, `GET /token?scope=sample-api-a:write`.
In the handler ([`cmd/agent-sidecar/main.go`](../../cmd/agent-sidecar/main.go)),
a request with **no `subject_token` and no `audience`** falls through to the
default branch — a cached **`client_credentials`** mint (`tc.get(scope)`). The
other two branches are the delegation paths (exchange, and minting a
delegation token) and are not used here.

The scope itself comes from the agent's contract. For `global-worker` it is the
`SCOPE` env default in its template
([`agents/global-worker/template.json`](../../agents/global-worker/template.json)):

```json
"envDefaults": { "SCOPE": "sample-api-a:write" }
```

## 2. The request

`tc.get` does two things ([`cmd/agent-sidecar/main.go`](../../cmd/agent-sidecar/main.go)):

**Fetch a fresh JWT-SVID** from the SPIRE Workload API, with the `Audience` set
to the IdentityServer token URL. SPIRE shapes the SVID's `sub` from the pod's
labels using the ClusterSPIFFEID template
([`deploy/spire/clusterspiffeid.yaml`](../../deploy/spire/clusterspiffeid.yaml)):

```
spiffe://cluster.local/agent/{tenant-id}/{user-id}/{agent-type}/{agent-id}
```

So the SVID's subject is, for example,
`spiffe://cluster.local/agent/tenant-1/alice/global-worker/agent-22962c27`.

**POST to `/connect/token`** with the SVID as the client credential (RFC 7523
"private_key_jwt"–style client authentication):

```
grant_type=client_credentials
client_id=global-worker          # the agentType
client_assertion_type=urn:ietf:params:oauth:client-assertion-type:jwt-bearer
client_assertion=<the JWT-SVID>
scope=sample-api-a:write
```

The password-style `ClientSecrets` in
[`identityserver/Config.cs`](../../identityserver/Config.cs) are deliberately
placeholders — the SVID is the real credential.

## 3. The mint (server side)

IdentityServer processes the request in two custom stages, then emits the token.

**Authenticate the client.**
[`SpireClientSecretValidator`](../../identityserver/SpireClientSecretValidator.cs)
sees the `jwt-bearer` `client_assertion`, verifies the SVID against SPIRE's JWKS,
and resolves the client (`global-worker`), whose `AllowedScopes` in
[`Config.cs`](../../identityserver/Config.cs) include `sample-api-a:write`.
(Requests that carry a normal secret instead — e.g. the dashboard's human login —
are delegated to Duende's built-in validator.)

**Stamp the identity.**
[`AgentRegistryValidator`](../../identityserver/AgentRegistryValidator.cs) runs
*only* on the `client_credentials` path. It:

1. Extracts the agent id from the SVID's `sub` (the last path segment,
   `agent-22962c27`) and looks the agent up in the **registry**, requiring it to
   be `active` with a `userId`.
2. Adds `sub = user:<userId>` — the human principal the agent acts for. The
   `userId` was set at **spawn** (the dashboard injects the logged-in user →
   orchestrator pre-register → registry record).
3. Adds `act = { sub: <the SVID's spiffe id> }` — the agent as the actor.

**Emit the token.** Duende fills the rest: `iss` from the configured issuer
([`Program.cs`](../../identityserver/Program.cs) — the in-cluster
`http://identity-server:8080`, which the resource servers validate against),
`aud` from the **ApiResource** that owns the granted scope (`sample-api-a` owns
`sample-api-a:write`, per [`Config.cs`](../../identityserver/Config.cs)), a
default `exp`, and a random `jti`.

## 4. Worked example: the `global-worker` token

A token minted by this flow, decoded:

```json
{
  "iss": "http://identity-server:8080",
  "aud": "sample-api-a",
  "scope": ["sample-api-a:write"],
  "client_id": "global-worker",
  "sub": "user:alice",
  "act": {
    "sub": "spiffe://cluster.local/agent/tenant-1/alice/global-worker/agent-22962c27"
  },
  "iat": 1781052922,
  "exp": 1781056522,
  "jti": "43246C4DC2CB46EBAC446647EFEA59AE"
}
```

Where each claim comes from:

| Claim | Source |
| --- | --- |
| `iss` | IdentityServer's configured issuer ([`Program.cs`](../../identityserver/Program.cs)) |
| `client_id: global-worker` | the `client_id` in the request — the agentType |
| `scope: sample-api-a:write` | template `SCOPE` → sidecar `/token?scope=` → form `scope` |
| `aud: sample-api-a` | the ApiResource owning that scope ([`Config.cs`](../../identityserver/Config.cs)) |
| `sub: user:alice` | `AgentRegistryValidator`, from the registry record's `userId` (set at spawn) |
| `act.sub: spiffe://…/tenant-1/alice/global-worker/agent-22962c27` | the SVID's own `sub`, shaped by the [ClusterSPIFFEID](../../deploy/spire/clusterspiffeid.yaml) template from pod labels |
| `exp − iat = 3600` | Duende's default token lifetime (this client sets no override) |

The payoff: the resource server can see both *who* (`sub = user:alice`) and
*what is acting on their behalf* (`act` = the agent's SPIFFE identity), from a
token the agent obtained with nothing but its cryptographic workload identity.

## Where token exchange differs

The mint above is for an agent calling an API **with its own authority**. When a
**parent delegates to a child**, the child instead presents the delegation token
it was handed as the `subject_token` of an **RFC 8693 token exchange**, with its
own SVID as the `actor_token` — producing a token whose `act` chain is *extended*
(child on top of parent) and whose scope is *attenuated* to the parent's grant.
That path, and every check it enforces, is covered in
[Defining Policy → Delegation](/authoring/05-defining-policy#part-2--delegation).
In all cases the SVID is the client credential; only the **grant** changes.
