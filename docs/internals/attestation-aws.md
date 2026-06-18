---
title: Running on AWS with STS attestation
description: Stand up Spawnly on EKS using the AWS-STS attestor instead of SPIRE — IRSA wiring, one-command up/down scripts, and an end-to-end smoke test that proves attestation with zero SPIRE in the cluster.
---

# Running Spawnly on AWS with STS attestation

This runbook stands up Spawnly on **EKS** using the **AWS-STS attestor** instead
of SPIRE — proving the attestation seam (see [attestation.md](./attestation.md))
is pluggable. The kind/SPIRE `make bootstrap` path is unaffected; this is a
separate, AWS-native deploy.

> Cost: EKS bills hourly (control plane + nodes + NAT gateway). This is meant to
> be **ephemeral** — tear it down when done.

## Quick start (one command up / down)

Prereqs: AWS creds configured (see [../iam](../../deploy/aws/iam/) for the
least-privilege Terraform policy), plus `terraform`, `kubectl`, `docker`, `jq`.

```bash
# bring everything up and prove STS attestation end to end:
AWS_REGION=us-east-1 ./deploy/aws/up.sh

# re-run just the attestation smoke test later:
./deploy/aws/smoke-test.sh

# tear it ALL down so it stops costing money:
./deploy/aws/down.sh
```

`up.sh` runs `terraform apply` → kubeconfig → push images → deploy → spawns the
test worker and prints the STS evidence (registry `issuer=aws-sts` registration,
event timeline, and the worker's authorized `sample-api` result), exiting non-zero
if any step fails. The sections below document each step individually.

## How attestation works here

1. The operator runs agent pods as the IRSA-annotated `spawnly-agent`
   ServiceAccount and sets `AWS_ROLE_SESSION_NAME=<agentId>`.
2. The EKS IRSA webhook injects `AWS_ROLE_ARN` + a projected web-identity token.
3. The sidecar assumes the role and presents a **SigV4-presigned
   `sts:GetCallerIdentity`** request as its credential.
4. The registry and IdentityServer **replay it against AWS STS**; the returned
   assumed-role ARN's session name **is** the `AgentId`. AWS is the attestor.

**Trust boundary / caveat:** `RoleSessionName` is set by the workload, so the
per-agent name is *self-asserted* (AWS attests role possession, not the agentId).
This matches the demo threat model (platform-controlled agent images), the same
assumption SPIRE makes about the pod. Production hardening: anchor per-agent
identity in the cluster-attested projected-token `kubernetes.io.pod` claim (the
registry could verify that on self-registration) and cross-check it against the
STS session name via the AgentId-consistency invariant.

## Prerequisites

- AWS account + credentials (`aws configure` / `AWS_PROFILE`), AWS CLI v2,
  Terraform ≥ 1.5, `kubectl`, `docker`.
- The Terraform principal needs permissions to create VPC/EKS/IAM/ECR. Attach
  the least-privilege policy in [../iam](../../deploy/aws/iam/) (see its README)
  rather than `AdministratorAccess`.
- Verify access: `aws sts get-caller-identity` returns your account/ARN.

## 1. Provision infrastructure

```bash
cd deploy/aws/terraform
terraform init
terraform apply            # ~15 min for EKS
```

Note the outputs: `agent_role_arn`, `ecr_registry`, `region`, and run the
printed `kubeconfig_command`.

## 2. Build & push images to ECR

Builds every stage from the multi-stage Dockerfile and pushes to ECR (derives
the registry host from the Terraform output, handles `docker login`):

```bash
AWS_REGION=us-east-1 ./deploy/aws/push-images.sh
```

## 3. Deploy the platform

One script does it all: the `control-plane-auth` secret, the `ai-provider`
secret (from your env), the IRSA ServiceAccount annotation, the AWS overlay
(`ATTESTOR=aws-sts`, **no SPIRE / no `csi.spiffe.io`**), repointing images at
ECR, waiting for rollouts, and seeding templates with ECR-qualified agent
images. The registry writes its own SpiceDB schema on startup, so there is no
manual schema step.

```bash
export ANTHROPIC_API_KEY=sk-...      # or AI_API_KEY / OPENAI_API_KEY
AWS_REGION=us-east-1 ./deploy/aws/deploy.sh
```

## 4. Verify

Port-forward the dashboard/orchestrator and run a spawn → token → work flow (or
the e2e suite) with **zero SPIRE components present**. Confirm an agent's
`token_issued` event and a successful `sample-api` call: that is the AWS-STS
attestor end to end.

## 5. Teardown

```bash
kubectl kustomize --load-restrictor LoadRestrictionsNone deploy/aws | kubectl delete -f -
cd deploy/aws/terraform && terraform destroy
```
