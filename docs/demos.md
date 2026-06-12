---
title: See it in action
description: Short screen recordings of Spawnly on a live cluster — an agent working end to end, and chained agents with CIBA spawn consent and real-time revocation.
---

Two short clips of Spawnly running on a live cluster. Want to run them yourself?
Install the
[Claude Code plugin](https://github.com/dbfr3qs/Spawnly/blob/main/PLUGIN.md) and
run `/spawnly:up` then `/spawnly:demo` — Claude brings the platform up and
narrates the same scenarios on your own machine.

## An agent, working end to end

A long-lived `weather-monitor` agent: a user chats with it from the dashboard,
it calls a `get_weather` tool, and its lifecycle events stream into the timeline
in real time. Under the hood the agent never touches identity plumbing — its
sidecar fetched a SPIFFE SVID at startup, exchanged it for a scoped token, and
the agent just does its job.

<figure class="demo-video">
  <video controls preload="metadata">
    <source src="/media/general-demo.webm" type="video/webm" />
    <source src="/media/general-demo.mp4" type="video/mp4" />
    Your browser can't play embedded video —
    <a href="/media/general-demo.mp4">download the clip</a>.
  </video>
  <figcaption>Chatting with a long-lived agent while its event timeline streams below.</figcaption>
</figure>

How the chat path works: [Chatting with a long-lived agent](/authoring/06-chat).

## Chained agents: spawn consent + real-time revocation

The headline features, in one flow. A `chain-worker` spawns a chain of
sub-agents, and you see two things that make agent delegation safe:

- **Human-in-the-loop spawn consent (CIBA).** A child can't obtain credentials
  until a human **approves the spawn** on the dashboard — using OpenID CIBA, the
  backchannel approval flow banks use for out-of-band payment authorisation.
  Once granted, the stored consent auto-approves identical repeats deeper in the
  chain.
- **Real-time revocation cascade.** Revoke one node and its **entire descendant
  subtree** loses authorisation within seconds — live, while its ancestors keep
  working. Pods stay up; their next protected call simply returns `403`.
  Reversible with **resume**.

<figure class="demo-video">
  <video controls preload="metadata">
    <source src="/media/revoke-cascade.webm" type="video/webm" />
    <source src="/media/revoke-cascade.mp4" type="video/mp4" />
    Your browser can't play embedded video —
    <a href="/media/revoke-cascade.mp4">download the clip</a>.
  </video>
  <figcaption>A child waits for consent; once approved the chain grows; then revoking a node cascades <code>work_denied</code> through its subtree while ancestors keep serving <code>work_ok</code>.</figcaption>
</figure>

Go deeper: [CIBA spawn consent](/internals/spawn-consent) and
[Defining Policy](/authoring/05-defining-policy) (how the revoked relations are
derived).
