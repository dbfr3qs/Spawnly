# Public exposure — operator setup (one-time)

Step-by-step to put the dashboard on `spawnly.run`, IdentityServer on
`auth.spawnly.run`, and the docs on `docs.spawnly.run`. These are the **manual
account/registrar steps you run**; the design + what's automated is in
[../../docs/internals/public-exposure.md](../../docs/internals/public-exposure.md).

Run everything from the repo root with your AWS profile exported:

```bash
cd <repo-root>
export AWS_PROFILE=spawnly      # or your SSO profile
export AWS_REGION=us-east-1
```

> Cost: from step 6 on you run an ALB (~$16–23/mo) + the cluster + NAT
> continuously. `down.sh` keeps DNS/ECR (cheap re-up); `down.sh --all` destroys
> everything **including the hosted zone** (un-delegates the domain — don't run it
> casually once the registrar points at Route53).

---

## 0. Creds + (least-priv only) policy update
```bash
aws sts get-caller-identity --query Arn --output text   # confirm who you are
```
- **`spawnly-terraform` IAM user** (least-priv): publish the policy that gained
  `route53:*` + `acm:*`:
  ```bash
  ./deploy/aws/iam/render-policy.sh --apply
  ```
- **SSO AdministratorAccess role**: skip (admin already has it).

## 1. Note existing DNS records
At whatever manages `spawnly.run` DNS today, record only the entries **unrelated
to web serving** that you must keep — `MX` and email `TXT` (SPF/DKIM), and any
domain-verification `TXT`. You'll re-add just those in Route53.

**Do NOT re-add the apex `A spawnly.run` records** (the GitHub Pages
`185.199.108–111.153` IPs): the apex is being repurposed for the dashboard and is
created automatically by external-dns → ALB (step 6). Likewise the
`docs.spawnly.run` CNAME and the ACM-validation records are created by Terraform
(step 5). So unless you have email/verification records, step 2 may add **nothing**.

## 2. Create the Route53 zone + read its nameservers
```bash
terraform -chdir=deploy/aws/dns init
terraform -chdir=deploy/aws/dns apply -target=aws_route53_zone.primary
terraform -chdir=deploy/aws/dns output -json name_servers
```
If your GitHub user/org isn't `dbfr3qs`, set the docs target — add
`-var docs_pages_target=<youruser>.github.io` to the dns applies, or put it in
`deploy/aws/dns/terraform.tfvars`.

In the Route53 console for the new zone, **add back only the email/verification
records** from step 1 (`MX`, SPF/DKIM `TXT`, domain-verification `TXT`). Do **not**
create an apex `A`/`docs` record — those are created for you (external-dns in
step 6, Terraform in step 5).

## 3. Cut the registrar nameservers to Route53
At your **domain registrar**, replace `spawnly.run`'s nameservers with the 4 from
step 2. Then wait for propagation:
```bash
dig +short NS spawnly.run        # should list awsdns-*.* (minutes–hours)
```

## 4. Point docs at docs.spawnly.run (GitHub Pages)
In the GitHub repo (`dbfr3qs/Spawnly` → Settings → Pages):
- Set **Custom domain** = `docs.spawnly.run` (writes the `CNAME` file; TLS is
  provisioned once DNS resolves).
- Ask the maintainer/agent to update Astro `site: https://docs.spawnly.run` and
  redeploy (a code change — don't hand-edit).

> The docs currently live at the apex `spawnly.run`; afterwards they serve at
> `docs.spawnly.run` and the apex serves the dashboard once the cluster is up
> (step 6). Short window where the apex has no content in between.

## 5. Finish the DNS root (ACM cert validates after NS is live)
```bash
terraform -chdir=deploy/aws/dns apply        # completes once NS propagation lets ACM validate
terraform -chdir=deploy/aws/dns output -raw acm_certificate_arn   # prints an arn:aws:acm:... (not an error)
```
If the cert-validation step hangs, NS hasn't propagated — wait and re-run.

## 6. Bring the cluster up (edge auto-installs)
```bash
AWS_PROFILE=spawnly AWS_REGION=us-east-1 ./deploy/aws/up.sh
```
With the DNS root applied, `up.sh` sets `enable_public_edge=true`, installs the
AWS Load Balancer Controller + external-dns, deploys, applies the ingress (with
the ACM cert), and external-dns writes `spawnly.run` + `auth.spawnly.run` → ALB.
`deploy.sh` also wires the public OIDC origin (`OIDC_AUTHORITY` /
`DASHBOARD_ORIGIN` → `https://spawnly.run`, `FORWARDED_HEADERS=true` on the IdP)
and generates the dashboard login credential (next step) — no manual step needed.

> Single-origin design: the browser only ever talks to the apex, which
> reverse-proxies `/connect`,`/.well-known`,`/Account` to identity-server. So the
> public origin is the apex (`spawnly.run`), **not** `auth.spawnly.run`; the token
> `iss` stays the in-cluster URL. `auth.spawnly.run` is routed but unused by login.

## 7. Verify the edge + log in
```bash
dig +short spawnly.run            # → ALB hostname
dig +short auth.spawnly.run       # → ALB hostname
dig +short docs.spawnly.run       # → <user>.github.io
curl -sI https://spawnly.run | head -1                                          # 200/302 (dashboard)
curl -sI https://docs.spawnly.run | head -1                                     # docs over Pages TLS
```
Open `https://spawnly.run` in a browser and log in. The credential is **not**
`alice`/`alice` — `deploy.sh` generates a strong password into the `dashboard-user`
Secret (user `admin` by default; override with `DASHBOARD_USER=...`). It prints the
password once on first deploy; re-read it anytime with:
```bash
kubectl get secret dashboard-user -o jsonpath='{.data.password}' | base64 -d ; echo
```

That's it — the dashboard is fully public over HTTPS with OIDC login.

---

## Teardown
- `./deploy/aws/down.sh` — destroy the cluster; **keep** DNS + ECR (cheap re-up).
- `./deploy/aws/down.sh --all` — destroy **everything** incl. the hosted zone.
  Re-do steps 2–3 (zone + registrar NS) on the next setup.
