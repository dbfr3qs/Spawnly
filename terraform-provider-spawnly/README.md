# terraform-provider-spawnly

A Terraform provider for managing **agent templates** in the Spawnly registry's
control plane — config-as-code for the agent catalog.

> Status: vertical slice (provider + `spawnly_agent_template` resource). Data
> sources, validators, and acceptance tests land in subsequent phases.

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

## Example

See [`examples/agent-template`](examples/agent-template/main.tf):

```bash
cd examples/agent-template
terraform plan
terraform apply
terraform destroy   # auto-disables then deletes (registry blocks deleting an active template)
```

## Provider configuration

| Argument   | Env               | Description                                        |
|------------|-------------------|----------------------------------------------------|
| `endpoint` | `SPAWNLY_ENDPOINT`| Registry control-plane base URL (required).        |
| `token`    | `SPAWNLY_TOKEN`   | Shared-secret bearer (optional; omit when open).   |
