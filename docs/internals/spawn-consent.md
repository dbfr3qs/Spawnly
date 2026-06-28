---
title: CIBA spawn consent
description: How a parent template puts a human in the loop of a spawn — the child sidecar runs an OpenID CIBA backchannel authentication, the user approves on the dashboard, and the granted token is the child's user-bound access.
---

# CIBA spawn consent

Delegation has a built-in safety rail: a child's scopes must be a **subset of
its parent's** (the token-exchange policy). Handoff — spawning an agent with a
*different* skill set and therefore different scopes — cannot use that rail,
which opens the classic **confused deputy** gap: a parent could route work to a
child whose authority the user never sanctioned.

Spawn consent closes that gap with **CIBA** (OpenID Connect [Client-Initiated
Backchannel Authentication](https://openid.net/specs/openid-client-initiated-backchannel-authentication-core-1_0.html)):
the privilege jump is legitimate *because the user explicitly approved that
specific parent-type → child-type edge with those scopes*.

The mental model in one line:

> The child's sidecar runs a real OAuth **grant** whose approver is the human:
> no approval, no token — and the **granted token is the consent**.

## Switching it on

Consent is an option **per child type in the parent's template**
([`agents/chain-worker/template.json`](../../agents/chain-worker/template.json)):

```json
"delegation": {
  "allowedChildTypes": ["chain-worker"],
  "childPolicies": {
    "chain-worker": {"requireUserConsent": true, "consentTTL": "720h"}
  }
}
```

The child's template declares the scopes consent covers:

```json
"oauthScopes": ["openid", "sample-api-a:read"]
```

Two IdentityServer prerequisites for a consent-gated child type
([`identityserver/Config.cs`](../../identityserver/Config.cs)): its client
entry must allow the CIBA grant (`urn:openid:params:grant-type:ciba`) and the
`openid` scope (CIBA is an OIDC *authentication* request).

## The flow

1. **Spawn.** The orchestrator's spawn-policy check returns `consentRequired`
   (from the parent template's `childPolicies`) and stamps it onto the
   AgentWorkload; the operator surfaces it to the pod as `CONSENT_REQUIRED`
   plus `CONSENT_SCOPES`.

2. **Backchannel request.** The child's sidecar self-registers, flips its
   registry record to `awaiting-consent`, and POSTs to `/connect/ciba` — its
   JWT-SVID as the client assertion, `login_hint` = the spawning user,
   `scope` = the template-declared set
   ([`cmd/agent-sidecar/ciba.go`](../../cmd/agent-sidecar/ciba.go)).

3. **Edge binding.** IdentityServer resolves the **spawn edge entirely from
   the registry record behind the SVID** — agent → user + parent — and rejects
   mismatched `login_hint`s, revoked agents, and parentless spawns. Nothing
   about the edge is trusted from request parameters
   ([`identityserver/CibaRequestValidator.cs`](../../identityserver/CibaRequestValidator.cs)).

4. **Ask or skip.** The notification hook checks the registry's consent store
   for a grant covering `(user, parentType, childType)` and the requested
   scopes ([`identityserver/CibaConsentNotificationService.cs`](../../identityserver/CibaConsentNotificationService.cs)):
   - **covered** → the request auto-completes; the sidecar's first poll
     returns tokens and no human is involved;
   - **not covered** (first time, scope escalation, TTL expiry, or revoked) →
     the request stays pending, the dashboard shows an approve/deny prompt,
     and the optional `NOTIFIER_WEBHOOK_URL` is pinged.

5. **The user decides** on the dashboard. The consent API is authenticated by
   the user's own IdentityServer session cookie, so the approver *is* the user
   the request asks to authenticate
   ([`identityserver/CibaConsentApi.cs`](../../identityserver/CibaConsentApi.cs)).
   Approval records the grant in the registry (expiry derived from the
   template's `consentTTL`) and completes the request; denial completes it
   with no scopes.

6. **Verdict at the sidecar.** The poll returns:
   - **tokens** → status `active`, `consent_granted` event; the access token —
     `sub` = the user, the agent in the `act` chain — is what `/token` serves
     to the agent;
   - **`access_denied` / `expired_token`** → `consent_denied` events to the
     agent *and its parent* (mirroring `spawn_denied`) and status `failed`
     (dropping SpiceDB authority). The sidecar stays up answering `403` —
     exiting would only make the kubelet restart it (native sidecars restart
     regardless of pod restart policy), and the registry refuses to
     re-register an agent whose authority was dropped.

While pending, `/token` answers `503` (the SDKs already retry 5xx), and the
`client_credentials` path refuses too — the record isn't `active`. The agent
code needs **no changes**: the gate lives where tokens are dispensed.

## Renewal is re-consent

The user-bound token is short-lived, and every renewal **re-runs the grant**.
While the stored consent stands, renewals auto-approve on the first poll — the
agent never notices. The moment the user revokes the consent (dashboard →
Consents → Revoke), the next renewal goes *pending* instead: token issuance
stops within the token lifetime, the re-consent prompt surfaces on the
dashboard, and a re-approval restores the agent **without restarting
anything**. Revocation of live access and re-prompting of future spawns are
the same mechanism. Denying the re-consent prompt is terminal, exactly like a
denial at spawn: `consent_denied` events and status `failed`.

Requests for scopes outside the consented set are refused locally by the
sidecar (`403`) — scope escalation always goes back to the human.

## Seeing it

`chain-worker` demonstrates the full loop: the root (spawned by the user,
parentless — no consent needed) self-spawns a chain. The first link prompts on
the dashboard; once approved, every deeper link auto-approves from the stored
consent. Deny instead and the link fails while its parent keeps working. The
E2E spec walks all of it
([`e2e/tests/ciba-consent.spec.ts`](../../e2e/tests/ciba-consent.spec.ts)).

The same prompt can be answered from a phone instead of the dashboard — see
[Mobile CIBA consent](./mobile-ciba.md).
