---
title: Spawnly
description: A reference architecture for AI agent identity — SPIFFE workload identity, scoped OAuth tokens, human-in-the-loop spawn consent (CIBA), delegated authority, and real-time revocation across the whole agent lifecycle.
template: splash
hero:
  tagline: A reference architecture for AI agent identity — every agent a first-class workload with cryptographic identity, scoped tokens, and revocable, delegated authority.
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

Everything runs locally on a Kind cluster — no cloud account required.

<div class="run-callout">
  <div class="run-option">
    <p class="run-label">Fastest — Claude Code plugin</p>
    <pre>/spawnly:up    <span class="run-comment"># bring the platform up on a local Kind cluster</span>
/spawnly:demo  <span class="run-comment"># guided tour: spawn, chains, consent, revocation</span></pre>
    <p class="run-note">Install it from <a href="https://github.com/dbfr3qs/Spawnly/blob/main/PLUGIN.md">PLUGIN.md</a>.</p>
  </div>
  <div class="run-option">
    <p class="run-label">Manual</p>
    <pre>make bootstrap</pre>
    <p class="run-note">Same result, straight from the <a href="https://github.com/dbfr3qs/Spawnly">repository</a> Makefile.</p>
  </div>
</div>

## Go deeper

<div class="doc-paths">
  <div class="doc-path">
    <h3>Build an agent</h3>
    <p>A from-scratch path: the platform contract, then three worked agents.</p>
    <ol>
      <li><a href="/authoring/00-anatomy">Anatomy of an Agent</a></li>
      <li><a href="/authoring/01-job-and-exit">Job-and-exit: Price Reporter</a></li>
      <li><a href="/authoring/02-loop-until-stopped">Loop-until-stopped: Queue Worker</a></li>
      <li><a href="/authoring/03-parent-and-child">Parent → child: Trip Planner</a></li>
    </ol>
  </div>
  <div class="doc-path">
    <h3>Reference</h3>
    <p>Templates and policy, field by field, plus the chat contract.</p>
    <ul>
      <li><a href="/authoring/04-defining-a-template">Defining a Template</a></li>
      <li><a href="/authoring/05-defining-policy">Defining Policy</a></li>
      <li><a href="/authoring/06-chat">Chatting with a Long-Lived Agent</a></li>
    </ul>
  </div>
  <div class="doc-path">
    <h3>Under the hood</h3>
    <p>How the platform works beneath the SDK, traced end to end.</p>
    <ul>
      <li><a href="/internals/token-minting">How an agent's token is minted</a></li>
      <li><a href="/internals/spawn-consent">CIBA spawn consent</a></li>
    </ul>
  </div>
</div>
