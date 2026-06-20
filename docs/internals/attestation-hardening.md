---
title: Hardening AWS attestation (GetWebIdentityToken + EKS Pod Identity)
description: Spike findings and the implementation plan for cluster-attested, readable, STS-native per-agent identity — replacing the self-asserted RoleSessionName with an EKS-set kubernetes-pod-name principal tag surfaced in an AWS-signed JWT.
---

# Hardening AWS attestation

## Problem

The shipped AWS attestor (`ATTESTOR=aws-sts`) derives the agent id from the STS
**`RoleSessionName`**, which the workload sets itself (the operator passes
`AWS_ROLE_SESSION_NAME=<agentId>`). AWS attests *role possession*, not the agent
id — a compromised agent could assume the shared role under a different session
name and impersonate another agent. We want the agent id to be **cluster-attested**
(unforgeable by the workload) while staying readable by the platform.

## Spike result (decisive)

We probed AWS's new **outbound web identity federation** API,
`sts:GetWebIdentityToken`, from a pod running under **EKS Pod Identity**, calling
it with **no caller tags**. The returned (AWS-signed) JWT payload:

```json
{
  "aud": "spawnly-spike",
  "sub": "arn:aws:iam::ACCT:role/spawnly-podid-spike",
  "iss": "https://<uuid>.tokens.sts.global.api.aws",
  "https://sts.amazonaws.com/": {
    "principal_id": "arn:aws:iam::ACCT:role/spawnly-podid-spike",
    "principal_tags": {
      "kubernetes-pod-name": "podid-getwebid-spike-scwqs",
      "kubernetes-pod-uid":  "f7a73daa-…",
      "kubernetes-namespace": "default",
      "kubernetes-service-account": "podid-spike",
      "eks-cluster-name": "spawnly",
      "eks-cluster-arn": "arn:aws:eks:us-east-1:ACCT:cluster/spawnly"
    }
  }
}
```

`GetWebIdentityToken` **propagates the EKS Pod Identity session tags into the JWT
as `principal_tags`** — including `kubernetes-pod-name`, which we did not pass.
This is the ideal:

- **Attested** — EKS sets `kubernetes-pod-name`/`uid`; the workload can't forge
  them. Caller-supplied `--tags` land in a *separate* `request_tags` field
  (confirmed in Phase 1), so the verifier reading `principal_tags` cannot be
  spoofed.
- **Readable** — it's a normal JWT claim, validated against the account STS
  issuer's public JWKS (`<iss>/.well-known/jwks.json`, RS256).
- **STS-native** — an STS-issued token; the agent id is a tag on the STS
  principal (exactly the original goal).
- **Cheap** — one shared ServiceAccount + one Pod Identity association covers
  every agent; each pod's token still carries its own attested pod name. No
  per-agent IAM roles, no operator IAM-mutation power, no cross-check.

### Why this beats the alternatives

| Approach | Attested | Platform-readable | Per-agent IAM churn |
|---|---|---|---|
| `aws-sts` today (RoleSessionName) | ❌ self-asserted | ✅ (ARN) | none |
| Design A (per-agent IAM role) | ✅ | ✅ (ARN) | **high** (role/SA per agent + operator IAM power) |
| Design B (cluster-signed SA token claim) | ✅ | ✅ (token) | none, but not STS-native |
| **GetWebIdentityToken + Pod Identity** | ✅ (EKS-set tag) | ✅ (JWT claim) | **none** |

## Design

`ATTESTOR=aws-stsweb` (new). Requires the account-level **outbound web identity
federation** feature and **EKS Pod Identity**.

1. **Operator** runs each agent pod as a shared ServiceAccount (e.g.
   `spawnly-agent`) that has a **Pod Identity association** to an IAM role. Pods
   are named deterministically `<agentId>-pod` (already the case).
2. **EKS Pod Identity** injects credentials and stamps the session with
   `kubernetes-pod-name=<agentId>-pod` (+ uid/ns/sa/cluster).
3. **Sidecar** calls `sts:GetWebIdentityToken(audience="spawnly",
   signingAlgorithm=RS256)` — **no `--tags`** — and presents the returned JWT as
   `client_assertion` (`jwt-bearer`).
4. **Verifier** (registry self-registration + IdentityServer token minting)
   validates the JWT against the STS issuer JWKS, checks `aud=="spawnly"`, then
   derives `agentId = principal_tags["kubernetes-pod-name"]` minus the `-pod`
   suffix. For defense in depth it also asserts `principal_tags`
   `kubernetes-namespace` / `kubernetes-service-account` / `eks-cluster-arn`
   match the expected values.

**Security-critical:** the verifier MUST read `["https://sts.amazonaws.com/"].principal_tags`
(EKS-set, attested), never `request_tags` (caller-set, self-asserted). The
AgentId-consistency invariant holds automatically: registry and IS extract the
same `kubernetes-pod-name`, and it equals the orchestrator's pre-registered
`aw.Name`.

**Consistency invariant (keep Go ↔ C# in lock step).** The Go
`registrant.identityFromTags` and the C# `StsWebCredentialVerifier` must derive
byte-identical `AgentID`/`Subject`/`Issuer` from the same `principal_tags`. Two
subtleties to preserve when editing either side:
- The `Subject` is path-style (`<eks-cluster-arn>/agent/<agentId>`) so downstream
  act-chain handling recovers the agentId via the last path segment.
- A **missing _or_ empty** `eks-cluster-arn` falls back to the literal `"eks"` on
  both sides. (Go has no Go test project gap here; the C# side has no unit-test
  project today — if one is added, lock this with an empty-vs-missing case.)

**Threat model / parity with SPIRE.** EKS (the control plane + Pod Identity
agent) attests the pod identity, the same trust root SPIRE uses (kubelet/node).
A container can only obtain its own pod's session, so it can only ever present
its own attested `kubernetes-pod-name`. Residual: short-TTL token replay before
expiry, mitigated by TTL + `aud` binding — not worse than SPIRE.

## Implementation plan

### Phase 0 — Prerequisites
- Confirm `aws-sdk-go-v2/service/sts` exposes `GetWebIdentityToken`; `go get -u`
  the sts module if needed. (If the Go SDK lags, fall back to a SigV4-signed HTTP
  call — but verify the SDK first.)
- Account: `aws iam enable-outbound-web-identity-federation` (one-time; idempotent).
  Capture the issuer via `aws iam get-outbound-web-identity-federation-info`
  (`IssuerIdentifier`). This becomes `STSWEB_ISSUER`.

### Phase 1 — Sidecar credential source (Go)
- `internal/attestor/stsweb.go`: `StsWebSource{ audience string }` whose `Fetch`
  calls `GetWebIdentityToken` (Audience=[audience], SigningAlgorithm=RS256,
  DurationSeconds=3600, no Tags) and returns
  `Credential{Value: *out.WebIdentityToken, AssertionType: JWTBearerAssertionType}`.
  Creds come from Pod Identity via the default credential chain.
- Wire `case "aws-stsweb"` in `cmd/agent-sidecar/main.go`, reading
  `STSWEB_AUDIENCE` (default `spawnly`).
- Unit-test the credential shape with a faked STS client.

### Phase 2 — Registry verifier (Go)
- `internal/registrant/stsweb.go`: `StsWebVerifier` that validates the bearer JWT
  against the STS issuer JWKS (reuse the `jwx` JWKS cache as in `oidc.go`),
  checks `aud`, extracts `["https://sts.amazonaws.com/"].principal_tags.kubernetes-pod-name`,
  strips `-pod` → `AgentID`; asserts ns/sa/cluster claims; `Issuer="aws-stsweb"`.
- Config: `STSWEB_ISSUER`, `STSWEB_AUDIENCE`, expected `STSWEB_NAMESPACE` /
  `STSWEB_SERVICE_ACCOUNT` / `STSWEB_CLUSTER_ARN`.
- `cmd/registry/main.go`: add `case "aws-stsweb"` (verifier) and the
  `attestorDefault` mapping (`aws-stsweb` → `aws-stsweb`).
- Confirm `validAgentID` still accepts the derived id.

### Phase 3 — IdentityServer verifier (C#)
- `identityserver/StsWebCredentialVerifier.cs : IAgentCredentialVerifier`:
  validate the JWT against the STS issuer JWKS (pattern of `SpireSvidValidator`),
  check `aud`, extract `principal_tags.kubernetes-pod-name` → `AgentId`
  (strip `-pod`), assert ns/sa/cluster; `Issuer="aws-stsweb"`.
- `Program.cs`: `case "aws-stsweb"` selecting it, reading `STSWEB_*` env.
- `AgentClientSecretValidator` already accepts `jwt-bearer` — no change.

### Phase 4 — Operator injector + selector (Go)
- `internal/operator/identity.go`: `StsWebInjector{ ServiceAccount, Region,
  Audience }` — sets `serviceAccountName` and stamps `ATTESTOR=aws-stsweb`,
  `AWS_REGION`, `STSWEB_AUDIENCE` on the sidecar. **No IRSA annotation, no
  AWS_ROLE_SESSION_NAME** (Pod Identity owns the session; the EKS webhook injects
  the AWS creds env automatically).
- `cmd/operator/main.go`: `case "aws-stsweb"` building it.

### Phase 5 — Infra + scripts
- **Terraform** (`deploy/aws/terraform/`):
  - `aws_eks_addon "eks-pod-identity-agent"`.
  - Agent IAM role: trust `pods.eks.amazonaws.com` with `sts:AssumeRole` +
    `sts:TagSession`; inline policy `sts:GetWebIdentityToken`. (Replaces the
    IRSA web-identity trust on the agent role.)
  - `aws_eks_pod_identity_association` (cluster, `default`, `spawnly-agent`, role).
  - Enable outbound web identity federation: use a native resource if the
    provider supports it; otherwise a `null_resource` `local-exec` calling
    `aws iam enable-outbound-web-identity-federation`, with the issuer read back
    via an `external` data source. Expose `output "stsweb_issuer"`.
  - Keep `enable_cluster_creator_admin_permissions`; **also add an `access_entries`
    block** mapping the SSO admin role (and/or `spawnly-terraform`) so the
    SSO access-entry mismatch can't recur.
- **`deploy.sh`**: set `ATTESTOR=aws-stsweb` on operator/registry/identity-server;
  inject `STSWEB_ISSUER` (from the TF output or `get-outbound-web-identity-federation-info`),
  `STSWEB_AUDIENCE=spawnly`, and the expected ns/sa/cluster-arn; create the
  plain `spawnly-agent` SA (no IRSA annotation). Drop the IRSA `serviceaccount.yaml`
  role-arn step.
- **`up.sh`**: after `terraform apply`, ensure outbound federation is enabled and
  read the issuer; **add an access-entry self-heal** (create/associate admin for
  the running caller ARN, handling the SSO `assumed-role`→role-ARN conversion) so
  `kubectl` always works post-apply regardless of SSO; pass `STSWEB_ISSUER` to
  `deploy.sh`.
- **`down.sh`**: unchanged for teardown (the addon, association, and role are now
  Terraform-managed and destroyed with the cluster). Note (do not auto-run) that
  `aws iam disable-outbound-web-identity-federation` is the optional account-level
  revert — left enabled by default since it's a harmless account capability.

### Phase 6 — Verify on a cluster
- Extend `smoke-test.sh` to assert the agent registered with `issuer=aws-stsweb`
  and that the agent id came from the attested pod name.
- **Spoof test:** spawn an agent whose sidecar also passes a bogus
  `--tags kubernetes-pod-name=someone-else`; confirm it lands in `request_tags`
  and is ignored — the verifier still derives the real id from `principal_tags`.

### Phase 7 — Docs + deprecate
- Update `attestation.md` / `attestation-aws.md` for the `aws-stsweb` path.
- Mark `aws-sts` (GetCallerIdentity / RoleSessionName) legacy: readable but
  self-asserted; keep behind the selector for non-Pod-Identity environments.

## Status

Implemented and **verified end-to-end on EKS** (`issuer=aws-stsweb`,
`token_issued`, `work_ok`, no SPIRE), including a spoof test proving a forged
`request_tags.kubernetes-pod-name` is ignored. Resolved during implementation:
Go SDK `v1.43.3` already has `GetWebIdentityToken`; outbound federation is enabled
via `up.sh` (not Terraform); the Go verifier needs
`jws.WithInferAlgorithmFromKey(true)` because the AWS STS JWKS RSA key omits `alg`.

ECR now lives in its own Terraform root (`deploy/aws/ecr`) so images persist
across `down.sh`/`up.sh` — `down.sh` destroys only the cluster root. (Done; was a
backlog item.)
