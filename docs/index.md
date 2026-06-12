---
title: Spawnly
description: A reference architecture for AI agent identity — SPIFFE workload identity, scoped OAuth tokens, human-in-the-loop spawn consent (CIBA), delegated authority, and real-time revocation across the whole agent lifecycle.
template: splash
hero:
  tagline: A reference architecture for AI agent identity. SPIFFE workload identity, scoped OAuth tokens, human-in-the-loop spawn consent, and real-time revocation — across the whole agent lifecycle.
  image:
    html: |
      <video class="hero-video" autoplay loop muted playsinline controls preload="metadata" aria-label="A user chatting with a long-lived Spawnly agent on the dashboard">
        <source src="/media/general-demo.webm" type="video/webm" />
        <source src="/media/general-demo.mp4" type="video/mp4" />
      </video>
  actions:
    - text: See it in action
      link: /demos
      icon: right-arrow
      variant: primary
    - text: Start with the anatomy
      link: /authoring/00-anatomy
      icon: open-book
      variant: secondary
    - text: View on GitHub
      link: https://github.com/dbfr3qs/Spawnly
      icon: external
---

## The problem

AI agents increasingly spawn other agents and call protected services on a
user's behalf. A static API key can't express **which** agent is acting, **for
whom**, or **with what authority** — and it doesn't attenuate as work is handed
down a delegation chain. Spawnly is a working answer: every agent is a
first-class workload with a cryptographic identity, and authority flows — and is
revoked — along the chain.

It's a proof-of-concept platform, built to make these ideas concrete and
runnable, not a product. Agents can be short-lived (do one job and exit) or
long-lived (serve until deleted, including chat).

## What it demonstrates

<div class="feature-grid">
  <a class="feature-card" href="/internals/token-minting">
    <h3>Per-pod workload identity → scoped tokens</h3>
    <p>Every agent gets a unique SPIFFE JWT-SVID from SPIRE at startup — no shared secrets — and a sidecar exchanges it for scoped OAuth tokens, so agent code carries zero identity plumbing.</p>
    <span class="feature-link">How a token is minted →</span>
  </a>
  <a class="feature-card" href="/internals/spawn-consent">
    <h3>Human-in-the-loop spawn consent</h3>
    <p>Sub-agent spawning can be gated on user approval via OpenID CIBA, with stored consent and auto-approval on repeats.</p>
    <span class="feature-link">CIBA spawn consent →</span>
  </a>
  <a class="feature-card" href="/authoring/05-defining-policy">
    <h3>Delegated, attenuated authority</h3>
    <p>Parent → child agent chains, with per-template delegation policy that narrows authority as work is handed down.</p>
    <span class="feature-link">Defining policy →</span>
  </a>
  <a class="feature-card" href="/demos">
    <h3>Real-time revocation cascade</h3>
    <p>Revoke an agent and its entire descendant subtree loses authority within seconds; pods stay up, their next call returns 403. Reversible.</p>
    <span class="feature-link">See it in action →</span>
  </a>
  <div class="feature-card">
    <h3>Relationship-based authorisation</h3>
    <p>SpiceDB relations written at registration and checked by protected APIs; tenanted and global agents on one code path.</p>
  </div>
  <div class="feature-card">
    <h3>Full lifecycle observability</h3>
    <p>Every component emits structured events into an append-only, per-agent timeline.</p>
  </div>
</div>

## Run it yourself

The fastest path is the
[Claude Code plugin](https://github.com/dbfr3qs/Spawnly/blob/main/PLUGIN.md):
`/spawnly:up` brings the platform up on a local Kind cluster, and `/spawnly:demo`
walks you through these scenarios live — spawn, chains, consent, and a revocation
cascade. Prefer the manual path? `make bootstrap` does the same from the
[repository](https://github.com/dbfr3qs/Spawnly).

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
