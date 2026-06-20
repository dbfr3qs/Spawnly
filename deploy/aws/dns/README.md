# DNS + TLS (persistent Terraform state)

The Route53 hosted zone for the domain, the ACM certificate (apex + wildcard),
and the `docs.<domain>` → GitHub Pages CNAME live in this **own root/state**,
**independent of the cluster** — like `deploy/aws/ecr`. The registrar delegates
nameservers here, so this must persist across cluster `down`/`up`.

- **Applied once, manually** via `terraform` (see first-time setup below) — not
  by `up.sh`. It rarely changes after the one-time NS cutover.
- **NOT destroyed by** `down.sh`. It is only torn down by an explicit full
  teardown: `./deploy/aws/down.sh --all` (which also breaks the registrar NS
  delegation — you'd re-delegate on the next setup).

## First-time setup (one-time NS cutover)

ACM DNS validation only completes once the validation records resolve publicly,
i.e. after the registrar points at this zone's nameservers. So:

```bash
# 1. Create the zone (and cert/records); the cert-validation step will wait.
terraform -chdir=deploy/aws/dns init
terraform -chdir=deploy/aws/dns apply -target=aws_route53_zone.primary

# 2. Point the registrar's nameservers at the zone:
terraform -chdir=deploy/aws/dns output -raw name_servers   # set these at your registrar

# 3. Finish — cert validates once NS propagates (minutes to ~an hour):
terraform -chdir=deploy/aws/dns apply
```

Before flipping NS, replicate any existing records you still need (e.g. the
current docs/apex records) into this zone so nothing goes dark during the cutover.

## Outputs
- `name_servers` — set at the registrar (one-time).
- `acm_certificate_arn` — consumed by the ALB ingress.
- `zone_id` — used by external-dns / record management.
