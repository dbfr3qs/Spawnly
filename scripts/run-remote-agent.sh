#!/usr/bin/env bash
# scripts/run-remote-agent.sh — launch a bare in-cluster Pod that SIMULATES a
# consumer-hosted (remote) agent for the Phase-10 e2e (remote-agent.spec.ts).
#
# ⚠ E2E ONLY. The Pod runs the real agent-sidecar in remote mode
# (AGENT_PLACEMENT=remote, ATTESTOR=aws-sts) plus the remote-demo agent, but with
# NO AgentWorkload CR and NO SPIRE socket — exactly a consumer-hosted agent. It
# presigns GetCallerIdentity against the in-cluster fake STS (AWS_ENDPOINT_URL_STS)
# with a fake ambient access key that maps to a configured principal, and carries
# AWS_ROLE_SESSION_NAME=<agentId> so the fake echoes the agentId as the STS session
# name (the id the verifier binds to the org anchor).
#
# Usage: run-remote-agent.sh <podName> <agentId> <principal>
#   <principal> is the fake AWS access-key id (== a FAKE_STS_PRINCIPALS key):
#   acme-good (acme's account) or globex-good (a different account).
set -euo pipefail

POD="${1:?pod name required}"
AGENT_ID="${2:?agentId required}"
PRINCIPAL="${3:?principal required}"
AGENT_TYPE="${AGENT_TYPE:-weather-monitor}"

kubectl delete pod "$POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true

cat <<YAML | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: default
  labels:
    # Marks the Pod as an agent for the control-plane NetworkPolicies (registry +
    # keycloak ingress allow agent pods). A real consumer Pod is off-cluster; this
    # label is the in-cluster simulation's stand-in for that reachability.
    agent-platform.io/managed: "true"
    spawnly.run/e2e-only: "true"
    remote-demo: "true"
spec:
  restartPolicy: Never
  initContainers:
    - name: agent-sidecar
      image: agent-sidecar:latest
      imagePullPolicy: Never
      restartPolicy: Always
      env:
        - {name: AGENT_PLACEMENT, value: "remote"}
        - {name: ATTESTOR, value: "aws-sts"}
        - {name: AGENT_ID, value: "${AGENT_ID}"}
        - {name: AGENT_TYPE, value: "${AGENT_TYPE}"}
        - {name: USER_ID, value: "viewer"}
        - {name: REGISTRY_URL, value: "http://registry:8080"}
        - {name: IS_TOKEN_URL, value: "http://keycloak:8080/realms/spawnly/protocol/openid-connect/token"}
        # Point the presigner at the in-cluster fake STS and reflect the agentId as
        # the role-session-name so the fake returns an ARN whose session name IS the
        # agentId (see internal/attestor/aws.go, cmd/fake-sts).
        - {name: AWS_ENDPOINT_URL_STS, value: "http://fake-sts:8080"}
        - {name: AWS_ROLE_SESSION_NAME, value: "${AGENT_ID}"}
        # Fake ambient credentials — the access-key id selects the fake principal.
        - {name: AWS_ACCESS_KEY_ID, value: "${PRINCIPAL}"}
        - {name: AWS_SECRET_ACCESS_KEY, value: "fake-secret"}
        - {name: AWS_REGION, value: "us-east-1"}
  containers:
    - name: agent
      image: agent-remote-demo:latest
      imagePullPolicy: Never
      env:
        - {name: AGENT_ID, value: "${AGENT_ID}"}
        - {name: MINT_SCOPE, value: "openid"}
YAML
