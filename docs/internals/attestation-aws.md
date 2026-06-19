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

`up.sh` runs `terraform apply` → kubeconfig → access-entry self-heal → enable
outbound federation → push images → deploy → spawns the test worker and prints
the attestation evidence (registry `issuer=aws-stsweb` registration, event
timeline, and the worker's authorized `sample-api` result), exiting non-zero if
any step fails. The sections below document each step individually.

## How attestation works here (`aws-stsweb`)

The deployed attestor is **`aws-stsweb`** — STS outbound web-identity federation
(`sts:GetWebIdentityToken`) anchored on **EKS Pod Identity**. See
[attestation-hardening.md](./attestation-hardening.md) for the full design and
the spike that proved it.

1. The operator runs agent pods (`<agentId>-pod`) as the `spawnly-agent`
   ServiceAccount, which has an EKS **Pod Identity association** to an IAM role.
2. EKS injects AWS credentials and stamps **cluster-attested session tags**
   (`kubernetes-pod-name=<agentId>-pod`, uid, namespace, sa, cluster-arn) — the
   workload cannot forge these.
3. The sidecar calls `sts:GetWebIdentityToken` (audience `spawnly`, no caller
   tags) and presents the AWS-signed JWT as its credential.
4. The registry and IdentityServer validate the JWT against the account STS
   issuer's JWKS and derive `AgentId` from
   `principal_tags.kubernetes-pod-name` minus `-pod`.

**Why it's attested:** EKS sets `kubernetes-pod-name`; a malicious agent's
caller-supplied tags land in a separate `request_tags` field the verifier
ignores. This restores parity with SPIRE (the control plane attests the pod),
unlike the legacy `aws-sts` attestor whose `RoleSessionName` was self-asserted.

> The legacy `aws-sts` attestor (presigned `GetCallerIdentity`, self-asserted
> session name) remains available behind the selector for non-Pod-Identity
> environments, but `aws-stsweb` is the recommended, hardened path.

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
