---
title: Spawnly Documentation
description: Authoring and policy documentation for the Spawnly agent platform.
template: splash
hero:
  tagline: Build, run, and govern short-lived and long-lived agents with cryptographic identity, scoped tokens, and least-privilege delegation.
  actions:
    - text: Start with the anatomy
      link: /authoring/00-anatomy
      icon: right-arrow
      variant: primary
    - text: View on GitHub
      link: https://github.com/dbfr3qs/Spawnly
      icon: external
---

## Authoring guides

A from-scratch path through building an agent on the platform, followed by deep
reference on templates and policy.

- **[Anatomy of an Agent](/authoring/00-anatomy)** — the platform contract, injected environment, the SDK, and the build → register → spawn → observe loop.
- **[Job-and-exit: Price Reporter](/authoring/01-job-and-exit)** — spin up, do one job, exit.
- **[Loop-until-stopped: Queue Worker](/authoring/02-loop-until-stopped)** — long-lived, runs until deleted.
- **[Parent → child: Trip Planner & Currency Converter](/authoring/03-parent-and-child)** — orchestration over A2A with delegated, attenuated authority.
- **[Defining a Template](/authoring/04-defining-a-template)** — the full `AgentTemplate` schema, field by field.
- **[Defining Policy](/authoring/05-defining-policy)** — an agent's own authority and parent→child delegation.
- **[Chatting with a Long-Lived Agent](/authoring/06-chat)** — how the dashboard chat interface works and the `/agents/chat/:sessionId` contract.

## Under the hood

How the platform works beneath the SDK.

- **[How an agent's token is minted](/internals/token-minting)** — a workload's JWT-SVID becomes a scoped access token via `client_credentials`, with `sub = user:<id>` and an `act` actor — the non-exchange path, traced end to end.
- **[CIBA spawn consent](/internals/spawn-consent)** — putting a human in the loop of a handoff: the child sidecar runs an OpenID backchannel authentication, the user approves on the dashboard, and the granted token *is* the child's user-bound access.
