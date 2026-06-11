---
description: Bring the Spawnly platform up — preflight, bootstrap (or repair), and verify green
allowed-tools: Bash, Read
---

You are bringing the Spawnly platform up for the user. Preflight snapshot:

- docker: !`docker info >/dev/null 2>&1 && echo OK || echo "docker NOT running"`
- kind: !`command -v kind >/dev/null && kind --version || echo "kind MISSING"`
- kubectl: !`command -v kubectl >/dev/null && echo OK || echo "kubectl MISSING"`
- helm: !`command -v helm >/dev/null && echo OK || echo "helm MISSING"`
- existing cluster: !`kind get clusters 2>/dev/null | grep -q '^agent-platform$' && echo "EXISTS" || echo "none"`
- AI key present: !`( [ -f .env ] && grep -qE '^(ANTHROPIC_API_KEY|OPENAI_API_KEY|AI_API_KEY)=.+' .env ) && echo "in .env" || ( [ -n "$ANTHROPIC_API_KEY$OPENAI_API_KEY$AI_API_KEY" ] && echo "in shell env" || echo "MISSING — agents that chat will fail" )`

Steps:

1. **Preflight.** If docker/kind/kubectl/helm is missing, stop and tell the user
   what to install (point to the repo README Prerequisites). If the AI key is
   missing, warn that chat/coding agents won't work but bootstrap can proceed
   (set `ANTHROPIC_API_KEY` in `.env` to fix).

2. **Branch on cluster state:**
   - **No cluster** → run `make bootstrap`. This is long (Kind + SPIRE + image
     builds + deploy + seed); stream progress and be patient.
   - **Cluster EXISTS** → don't rebuild from scratch. Run `/spawnly:doctor`-style
     checks; if healthy, just ensure templates are seeded (`make reseed` if
     `GET /v1/templates` is empty) and report it's already up. If unhealthy,
     offer to re-run `make bootstrap` (it now rolls deployments to pick up fresh
     images and resets the in-memory stores).

3. **Verify green.** After bootstrap, confirm the core deployments are Ready
   (registry, identity-server, orchestrator, operator, dashboard) and templates
   are seeded. If anything failed, hand off to the `/spawnly:doctor` logic
   (especially the stuck spire-oidc → registry crashloop case).

4. **Done.** Tell the user the cluster is ready, that the dashboard is reachable
   via `make dash` (http://localhost:8090), and suggest `/spawnly:demo` next.

Use the spawnly-platform skill for the gotchas. Confirm before destructive
actions (e.g. tearing down and recreating a cluster).
