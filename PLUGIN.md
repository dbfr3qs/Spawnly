# Spawnly — Claude Code plugin

A companion plugin that makes running, demoing, and understanding Spawnly easy
from inside [Claude Code](https://claude.com/claude-code). It wraps the repo's
own `make` targets and APIs in guided, state-aware commands, and adds an ambient
"expert" skill so you can just *ask* how the platform works.

## Install

From a checkout of this repo:

```
/plugin marketplace add dbfr3qs/Spawnly
/plugin install spawnly
```

(or `/plugin marketplace add .` if you're pointing at a local clone.)

## Get going in two commands

```
/spawnly:up        # preflight + bootstrap (or repair) + verify green
/spawnly:demo      # guided, narrated tour using the example agents
```

`/spawnly:up` builds the Kind cluster, SPIRE, IdentityServer, the control plane,
and seeds the example agent templates. `/spawnly:demo` then walks you through the
platform's headline behaviours on the agents that ship with the repo.

## Commands

| Command | What it does |
|---|---|
| `/spawnly:up` | Preflight (docker/kind/kubectl/helm/API key), then `make bootstrap` — or, on an existing cluster, repair + reseed instead of rebuilding. Ends with the dashboard URL. |
| `/spawnly:status` | At-a-glance health: services, agents by phase, templates, port-forwards. |
| `/spawnly:doctor` | Diagnoses the known failure modes (stuck SPIRE-oidc → registry crashloop, unseeded registry, stale images, kubeconfig drift, port-forward conflicts, baked dev keys) and proposes exact fixes. |
| `/spawnly:demo [n]` | Narrated scenario tour: (1) hello-world spawn, (2) chain + real-time revoke cascade, (3) CIBA spawn consent, (4) token-exchange handoff, (5) tenancy. |
| `/spawnly:explain [topic]` | Explains a concept (identity, token-minting, consent, delegation, revoke, tenancy, events) grounded in the real code, with an option to show it live on the cluster. |

## Just ask

The `spawnly-platform` skill loads automatically, so you don't need a slash
command to learn how things work. Try, in plain English:

- "How does an agent actually prove who it is?"
- "Why did my chain-worker get a 403 after I revoked the parent?"
- "What's the difference between revoke and kill here?"
- "What does the `act` claim in the access token mean?"

Answers are grounded in this repo's source (cited `file:line`) and can be
demonstrated live on the running cluster.

## Requirements

A working Spawnly dev environment: Docker, Kind, kubectl, Helm, and an AI
provider key in `.env` (`ANTHROPIC_API_KEY=...`) for agents that chat or code.
See the repo [README](README.md) for full prerequisites.
