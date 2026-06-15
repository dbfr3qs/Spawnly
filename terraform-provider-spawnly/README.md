# terraform-provider-spawnly

A Terraform provider for managing **agent templates** in the Spawnly registry's
control plane — config-as-code for the agent catalog.

> Status: v1 — the `spawnly_agent_template` resource plus the
> `spawnly_agent_template`, `spawnly_agent_templates`, and `spawnly_schema` data
> sources, with validators, examples, generated `docs/`, and unit + acceptance
> tests.

## Local dev install

This module lives outside the repo's `go.work` workspace and builds standalone:

```bash
cd terraform-provider-spawnly
make install
export TF_CLI_CONFIG_FILE=$(pwd)/dev.tfrc
```

`make install` builds the binary into `./bin` and writes `dev.tfrc`, a CLI config
with a `dev_overrides` block. Under dev overrides you **skip `terraform init`** —
just run `plan`/`apply`.

## Pointing it at a registry

The provider talks directly to the registry control-plane API with the
shared-secret bearer:

```bash
# port-forward the in-cluster registry
kubectl port-forward svc/registry 18080:8080 &

# pull the bootstrap-generated control-plane token
export SPAWNLY_TOKEN=$(kubectl get secret control-plane-auth \
  -o jsonpath='{.data.token}' | base64 -d)

export SPAWNLY_ENDPOINT=http://localhost:18080   # or set `endpoint` in HCL
```

Against a registry running open (`CONTROL_PLANE_AUTH=none`) leave `SPAWNLY_TOKEN`
unset.

## Examples

- [`examples/agent-template`](examples/agent-template/main.tf) — minimal
  short-lived worker template.
- [`examples/agent-template-delegation`](examples/agent-template-delegation/main.tf)
  — richer template exercising `authz_template`, `delegation` (allowed child
  types, grantable scopes, `max_depth`, and a `child_policies` map entry gating
  a child spawn behind CIBA consent) and `oauth_scopes`.

```bash
cd examples/agent-template
terraform plan
terraform apply
terraform destroy   # auto-disables then deletes (registry blocks deleting an active template)
```

## Tests

`make test` runs the unit tests (no cluster needed). `make testacc` runs the
acceptance tests, which drive the real resource lifecycle (create → import →
update → destroy) against a live registry. They are gated on `TF_ACC` and read
`SPAWNLY_ENDPOINT` / `SPAWNLY_TOKEN` from the environment:

```bash
export SPAWNLY_ENDPOINT=http://localhost:18080
export SPAWNLY_TOKEN=...   # omit against an open registry
make testacc
```

## Docs

`make docs` regenerates `docs/` from the schema, `templates/`, and `examples/`
via [`tfplugindocs`](https://github.com/hashicorp/terraform-plugin-docs) (pinned
in `tools/tools.go`). Re-run it whenever a schema or example changes.

## Provider configuration

| Argument   | Env               | Description                                        |
|------------|-------------------|----------------------------------------------------|
| `endpoint` | `SPAWNLY_ENDPOINT`| Registry control-plane base URL (required).        |
| `token`    | `SPAWNLY_TOKEN`   | Shared-secret bearer (optional; omit when open).   |
