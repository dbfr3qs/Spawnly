---
description: Diagnose and fix common Spawnly cluster breakage
allowed-tools: Bash, Read
---

You are diagnosing Spawnly cluster health. Live snapshot:

- Cluster: !`kind get clusters 2>/dev/null || echo "no kind"`
- Pods: !`kubectl get pods 2>/dev/null | head -30 || echo "kubectl unavailable"`
- SPIRE pods: !`kubectl get pods -n spire-system 2>/dev/null | head || echo "no spire-system ns"`
- registry log tail: !`kubectl logs deploy/registry --tail=15 2>/dev/null || echo "registry not running"`
- identity-server log tail: !`kubectl logs deploy/identity-server --tail=15 2>/dev/null || echo "identity-server not running"`
- port-forwards: !`pgrep -af 'kubectl port-forward' 2>/dev/null || echo "none"`

Using the spawnly-platform skill's "operational gotchas", diagnose against the
known failure modes and report what you find. Check specifically for:

1. **Stuck `spire-spiffe-oidc-discovery-provider`** (Unknown/NotReady) →
   registry crashloops on its JWKS fetch. Fix: force-delete the stuck pod,
   then `kubectl rollout restart deploy/registry`, then `make reseed`.
2. **Registry crashlooping / not Ready** — correlate with the SPIRE check above
   and the registry log tail.
3. **Unseeded registry** — if `GET /v1/templates` is empty but the registry is
   up (the in-memory store resets on restart). Fix: `make reseed`.
4. **Stale images** — deployments running old code after a code change without
   a rollout. Fix: `make redeploy-<svc>` (or re-run `make bootstrap`).
5. **kubeconfig drift** (devcontainer) — kubectl pointing at localhost:8080 /
   "connection refused". Fix: `kind export kubeconfig --name agent-platform`.
6. **Port-forward conflicts** — duplicate/leftover forwards on 8080/8090.
7. **Baked dev keys** — identity-server "Error unprotecting signing key" spam
   (`identityserver/keys/` was baked into the image; see `.dockerignore`).

For each problem found: name it, explain the cause in one line, and give the
exact fix command. **Ask for confirmation before running anything that
restarts, deletes, or rebuilds.** If everything is healthy, say so and point to
`/spawnly:status` or `/spawnly:demo`.
