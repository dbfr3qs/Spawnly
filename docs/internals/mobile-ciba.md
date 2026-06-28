---
title: Mobile CIBA consent
description: How a user answers a CIBA spawn-consent prompt from their phone — the registry fans a pending request out to the mobile-gateway, which pushes to the user's devices (FCM/APNs) or streams it (SSE), and the app approves through the gateway's user-scoped proxy.
---

# Mobile CIBA consent

[Spawn consent](./spawn-consent.md) puts a human in the loop of a privilege
jump: a consent-gated child's sidecar runs a CIBA backchannel grant and **no
token issues until the user approves**. Out of the box the only place to answer
is the dashboard web UI — which undercuts CIBA's premise of *out-of-band approval
on a device you control*. Mobile CIBA brings the prompt to the phone with the
same guarantees.

The mental model in one line:

> The phone is just another approver surface for the **same** registry-owned
> consent request — it adds a notification transport and a user-scoped proxy, not
> a new authority.

## The pieces

- **`mobile-gateway`** ([`cmd/mobile-gateway`](../../cmd/mobile-gateway)) — the
  single origin the app talks to. It **proxies** the app's consent actions to the
  orchestrator's existing user-scoped endpoints (which stay the authorization
  authority), and owns three net-new things: a per-user **device registry**, the
  **push fan-out** that consumes the registry webhook, and a per-user **SSE
  stream**.
- **`mobile/`** ([`mobile/`](../../mobile)) — an Expo / React Native app
  (iOS + Android, one codebase). Android builds on Linux; iOS is built on EAS's
  cloud macOS workers (you cannot build iOS on Linux).
- The **`mobile` IdP client** ([`identityserver/Config.cs`](../../identityserver/Config.cs))
  — a **public** OAuth client (authorization-code + PKCE, no secret), carrying the
  same `aud=orchestrator` + `orchestrator:read/write` delegated scopes the
  dashboard uses (never `orchestrator:spawn` — spawn stays agent-only).

## The flow

1. **Enrol.** The app logs in via PKCE (refresh token in OS secure storage),
   requests OS push permission, gets the **native** device token (raw FCM on
   Android, APNs on iOS — we broker push ourselves, not via Expo's service), and
   registers it: `POST /me/devices`. The device is keyed to the token's user —
   never a client-supplied id.

2. **A consent goes pending.** Exactly as in the dashboard flow, a consent-gated
   child's sidecar opens the CIBA request and the registry records it pending.
   The registry then fires `NOTIFIER_WEBHOOK_URL` — now pointed at the gateway —
   carrying the request **id** + `agentId` (added for the mobile deep-link).

3. **Fan-out.** The gateway's `/internal/notify` (a control-plane-authenticated
   webhook, **not** a user endpoint) publishes a minimal, secret-free event to
   the user's SSE subscribers and, under `NOTIFIER=fcmapns`, sends a push to each
   registered device. The push carries an opaque request id and a **generic**
   banner — no scopes, no binding message ride the wire.

4. **The user decides.** Tapping the push deep-links to the request; the app
   **re-fetches authoritative state** over the authed channel (it never trusts
   the push payload), shows the `parentType → childType` edge, scopes, and
   binding message, and the user approves (optionally **narrowing** the scopes)
   or denies — behind a **device biometric** gate. The action is a
   `POST /me/consent-requests/{id}/approve|deny` the gateway forwards to the
   orchestrator with the user's own token; ownership is enforced there.

5. **Verdict at the sidecar.** Unchanged from the dashboard path: the granted
   token activates the child, or a denial fails it.

Renewal-as-re-consent, scope-escalation re-prompts, and consent revocation
(`GET/POST /me/consents`) all behave exactly as the dashboard — the registry is
the single owner of the lifecycle; mobile is one more driver of the same
endpoints.

## Local vs AWS

- **Local (`make bootstrap`)**: `NOTIFIER=dev`. No Firebase/Apple credentials, no
  network to Apple/Google — the **SSE stream is the delivery** (the app, on an
  emulator, holds `/me/stream`). The whole approve loop is exercised by
  [`e2e/tests/mobile-ciba.spec.ts`](../../e2e/tests/mobile-ciba.spec.ts).
- **AWS**: `NOTIFIER=fcmapns`, real background push. The gateway is exposed at
  `mobile.spawnly.run` (its **public** port only); the FCM service-account JSON
  and APNs `.p8` are mounted from the `mobile-push-credentials` secret
  (operator-provided by `deploy/aws/deploy.sh`, never imaged or committed).

## Security notes

- **Two ports.** The user surface (`/me/*` + SSE) runs on `:8080`; the spoofable
  webhook (`/internal/notify`) runs on `:8081`, locked by NetworkPolicy to the
  registry alone. A rogue agent pod cannot forge a consent prompt at a user, even
  in the open (`AllowAll`) demo tier.
- **No secrets on the wire to the device.** The push/SSE event is an opaque id +
  generic text; the app always re-fetches the real request over TLS.
- **`userId` is always from the validated token**, never client input (devices,
  consent, and the SSE subscription are all scoped this way).
- **Operational caveat.** The registry→gateway webhook hop currently uses the
  **shared-secret** control-plane tier. An `oidc`-tier deployment must keep the
  gateway on shared-secret for this hop (and rely on the NetworkPolicy), or the
  notify is silently dropped.
