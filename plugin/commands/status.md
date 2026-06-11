---
description: At-a-glance health of the Spawnly cluster — services, agents, templates, port-forwards
allowed-tools: Bash, Read
---

You are reporting the health of the Spawnly platform. Here is a live snapshot:

- Cluster: !`kind get clusters 2>/dev/null | grep -q '^agent-platform$' && echo "agent-platform present" || echo "NO Kind cluster"`
- Platform pods: !`kubectl get pods 2>/dev/null | grep -vE '^agent-[0-9a-f]+-pod' | head -20 || echo "kubectl unavailable"`
- Agent pods (count by phase): !`kubectl get pods 2>/dev/null | grep -E '^agent-[0-9a-f]+-pod' | awk '{print $3}' | sort | uniq -c || true`
- Port-forwards: !`pgrep -af 'kubectl port-forward' 2>/dev/null | sed 's/^[0-9]* *//' || echo "none"`

Using the spawnly-platform skill for context:

1. Summarize cluster health in 3-5 lines: which platform services are Ready,
   anything CrashLooping/Pending/Unknown, and how many agents are running.
2. If the cluster is missing or services aren't up, say so plainly and suggest
   `/spawnly:up`.
3. If pods look unhealthy (CrashLoopBackOff, Unknown, registry not Ready),
   suggest `/spawnly:doctor`.
4. If everything is green, note the dashboard is reachable via `make dash`
   (localhost:8090) and suggest `/spawnly:demo`.

If the agents are accessible, optionally fetch a quick count from the API to
cross-check, but do not start long-running commands. Keep it short and scannable.
