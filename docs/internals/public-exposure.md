---
title: Exposing the dashboard & IdP on a domain (AWS)
description: Plan for serving the dashboard at spawnly.run and IdentityServer at auth.spawnly.run over HTTPS on EKS — Route53, ACM, an ALB via the AWS Load Balancer Controller, external-dns, and the OIDC issuer wiring — with docs moved to docs.spawnly.run on GitHub Pages.
---

# Exposing the dashboard & IdP on a domain (AWS)

Serve the **dashboard at `spawnly.run`**, **IdentityServer at `auth.spawnly.run`**,
and move the **docs to `docs.spawnly.run`** (GitHub Pages), all over HTTPS on EKS.

Decisions (chosen): DNS in **Route53** (move the zone), edge via the **AWS Load
Balancer Controller (ALB) + ACM**, dashboard kept behind **OIDC login with the
demo users replaced** before exposure.

## Topology
```
registrar NS ──► Route53 zone (spawnly.run)            [persistent: deploy/aws/dns]
  ├─ spawnly.run        ALIAS ─► ALB ─► dashboard:8080        (TLS: ACM)
  ├─ auth.spawnly.run   ALIAS ─► ALB ─► identity-server:8080  (TLS: ACM)
  └─ docs.spawnly.run   CNAME ─► <user>.github.io             (TLS: GitHub Pages)
ACM cert: spawnly.run + *.spawnly.run (DNS-validated)  [persistent: deploy/aws/dns]
ALB from one Ingress (AWS LB Controller); apex/auth records by external-dns.
```

Issuer model — already supported by the apps' split-horizon env knobs:
`ISSUER_URI = OIDC_AUTHORITY = https://auth.spawnly.run`,
`DASHBOARD_ORIGIN = https://spawnly.run`, `IDENTITY_INTERNAL_URL` stays in-cluster,
resource servers' `IS_ISSUER = https://auth.spawnly.run` (JWKS may stay internal).

## Persistence & teardown
Three Terraform roots with different lifecycles:
- `deploy/aws/dns` — **persistent** (registrar delegates NS here). Applied once
  (incl. a one-time NS cutover); not destroyed by `down.sh`.
- `deploy/aws/ecr` — **persistent** (images survive teardown).
- `deploy/aws/terraform` — the **cluster** (recreated each `up`).

`down.sh` destroys only the cluster; `down.sh --all` also destroys ECR and DNS
(full teardown — stops all cost, but breaks the NS delegation).

## Implementation phases

### Phase 1 — Persistent DNS + TLS (DONE)
`deploy/aws/dns`: Route53 zone, ACM cert (apex + wildcard, DNS-validated),
`docs.<domain>` CNAME → Pages. One-time NS cutover at the registrar (see that
root's README). `down.sh --all` tears it down.

### Phase 2 — Cluster edge controllers (cluster root + deploy) — IMPLEMENTED
Provisioned in `deploy/aws/terraform/edge.tf` (IAM + Pod Identity) and installed
by `deploy/aws/install-edge.sh` (Helm). `up.sh` runs the installer **only when the
DNS root is applied** (public exposure is opt-in).
- **AWS Load Balancer Controller**: the AWS-published IAM policy (as a `spawnly-lbc`
  customer-managed policy), an IAM role + Pod Identity association, install via
  Helm/EKS addon.
- **external-dns**: scoped `route53:ChangeResourceRecordSets`/list on the zone,
  role + Pod Identity association, install via Helm (`--domain-filter`,
  `--txt-owner-id`, `--policy=sync`).

### Phase 3 — Ingress + routing — IMPLEMENTED
`deploy/aws/ingress.yaml` (applied by `deploy.sh` when the DNS root is set up; it
injects the ACM `certificate-arn`). external-dns writes the records from the rule
hosts. **Phase 4 (issuer env + cookie/ForwardedHeaders code) is still required for
a working browser login** — until then the apps are reachable at the ALB but the
OIDC flow isn't wired to the public hostnames.

Original phase notes:
One ALB Ingress (`target-type: ip`, `scheme: internet-facing`, HTTP→HTTPS
redirect, `certificate-arn` = the dns root's ACM ARN):
`spawnly.run → dashboard:8080`, `auth.spawnly.run → identity-server:8080`, with
external-dns annotations to write the apex/auth records.

### Phase 4 — App config + code touches
- Env: IdP `ISSUER_URI`/`DASHBOARD_ORIGIN`; dashboard `OIDC_AUTHORITY` (keep
  `IDENTITY_INTERNAL_URL` internal); resource servers `IS_ISSUER`.
- IdP `Program.cs`: `UseForwardedHeaders` (trust the ALB `X-Forwarded-Proto`) +
  `Secure`/`SameSite` cookies on HTTPS (today only the HTTP `SameSite=Lax`
  workaround exists). **Deferred until this phase so it's tested behind the ALB —
  bundling untested cookie/proxy changes risks the working kind + aws-stsweb flows.**
- Dashboard `cmd/dashboard/auth.go`: `Secure` cookies when `X-Forwarded-Proto=https`.

### Phase 5 — Replace demo users
Replace hardcoded `TestUsers.cs` with real credentials from a Secret before the
IdP is internet-facing.

### Phase 6 — Move docs to docs.spawnly.run (do first, to free the apex)
GitHub Pages custom domain `docs.spawnly.run` (+ `CNAME` file); Astro `site:
https://docs.spawnly.run`; the CNAME record is in the dns root.

### Phase 7 — Scripts & lifecycle
`up.sh` reads the dns root's `acm_certificate_arn` for the ingress and installs
the controllers. `down.sh` deletes the Ingress first (so the ALB + its DNS
records are cleaned up) before destroying the cluster. (DONE: teardown ordering +
`--all`.)

### Phase 8 — IAM (least-privilege additions)
Terraform principal gains `route53:*` (or zone-scoped), `acm:*`, and IAM
create/attach for the `spawnly-lbc`/`spawnly-externaldns` roles+policies
(already `spawnly*`-scoped). `eks:*` and `PassRole→pods.eks` already present.

### Phase 9 — Verify
`dig` the three hosts; `https://auth.spawnly.run/.well-known/openid-configuration`
shows `issuer: https://auth.spawnly.run`; `https://spawnly.run` login → token
`iss=auth.spawnly.run` → spawn + sample-api succeed.

## Cost & caveats
- Always-on: ALB ~$16–23/mo + Route53 zone $0.50/mo + the running cluster/NAT.
  ACM is free. This is a persistent public endpoint, not the ephemeral model.
- Migration sequencing: stand up `docs.spawnly.run` on Pages and replicate
  existing records in Route53 **before** flipping registrar NS, so docs never goes
  dark. ACM validation only completes after the NS cutover.
