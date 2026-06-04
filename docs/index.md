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

## Design notes

- **[Delegation design (RFC 8693)](/delegation-design)** — the trust model and locked decisions behind token-exchange delegation.
- **[Delegation implementation plan](/delegation-implementation-plan)** — how the delegation milestones were executed.
- **[Spawn-time child-spawn policy](/design-spawn-time-child-spawn-policy)** — governing the parent→child edge independently of scope delegation.
