# Spawnly Mobile

A small Expo / React Native app that lets a user answer **CIBA spawn-consent
prompts** from their phone: receive a push (or, in dev, an SSE event) when one of
their agents needs authorization, review the parent→child edge and requested
scopes, and **approve (with an optional scope narrowing) or deny** — gated behind
a device biometric.

It talks only to the **mobile-gateway** (`/me/*`), which proxies consent actions
to the orchestrator's user-scoped endpoints. The app holds no special authority:
it authenticates the user via OAuth2 authorization-code + PKCE against the same
IdentityServer the dashboard uses (the public `mobile` client), and every action
is the user's own delegated token.

## Architecture at a glance

```
app ──PKCE──> IdentityServer (mobile client, aud=orchestrator, read/write scopes)
app ──Bearer─> mobile-gateway /me/consent-requests|consents|devices|stream
                     │ forwards the user token
                     └──> orchestrator (user-scoped) ──> registry
registry ──webhook──> mobile-gateway /internal/notify ──> push (FCM/APNs) + SSE
```

- `src/auth.ts` — PKCE login + silent refresh; tokens in OS secure storage (Keychain/Keystore).
- `src/api.ts` — the gateway client (`/me/*`). Always re-fetches authoritative state; never trusts a push payload.
- `src/push.ts` — OS permission + the **native** device push token (raw FCM/APNs), registered against the user.
- `src/sse.ts` — the dev/foreground stream (`/me/stream`), the only delivery under `NOTIFIER=dev`.
- `src/biometric.ts` + `src/consent-logic.ts` — the biometric gate and the pure, unit-tested approve/scope logic.
- `src/screens/*` — login, pending list, request detail (scopes + binding + narrowing), consents, settings.

## iOS on Linux — read this

You **cannot** build or sign an iOS binary on Linux; Xcode is macOS-only.
- **Android** builds locally on Linux (`eas build -p android --profile development`, or `expo run:android`).
- **iOS** is built on **EAS's cloud macOS workers** — no local Mac needed:
  `eas build -p ios --profile development`. The iOS simulator profile is in `eas.json`.
- v1 is **internal/dev distribution only** (EAS internal / TestFlight) — no public App Store / Play Store submission.

## Configure

`app.json → expo.extra` (or `EXPO_PUBLIC_*` env) sets the endpoints:

| Key          | Production            | Local (`make bootstrap`)        |
|--------------|-----------------------|---------------------------------|
| `issuer`     | `https://auth.spawnly.run` | your port-forwarded IdP/dashboard origin |
| `gatewayUrl` | `https://mobile.spawnly.run` | `http://<dev-host>:8091` (gateway public port-forward) |
| `clientId`   | `mobile`              | `mobile`                        |

For a local emulator, point `gatewayUrl`/`issuer` at your machine's LAN IP (an
Android emulator reaches the host at `10.0.2.2`). Real background push needs the
AWS path (`NOTIFIER=fcmapns`); locally the SSE stream delivers prompts while the
app is foregrounded.

## Develop & test

```bash
npm install
npm run typecheck     # tsc --noEmit
npm test              # jest — biometric gate + scope-narrowing logic
npm start             # Expo dev server
```

The push **keys live on the server** (the gateway's `mobile-push-credentials`
secret), never in the app bundle — see `.gitignore`.
