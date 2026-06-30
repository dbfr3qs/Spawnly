---
title: Config-as-code with Terraform
description: Manage the agent-template catalog declaratively with terraform-provider-spawnly, talking to the registry's shared-secret control plane — and how that control plane is authenticated.
---

# Config-as-code with Terraform

Agent templates — the catalog that says *what kinds of agent exist*, their image,
authority, and delegation policy — don't have to be seeded by a script. They can
be managed declaratively with the
[`terraform-provider-spawnly`](../../terraform-provider-spawnly/README.md)
provider, which talks **directly to the registry's control-plane API**.

This page covers the control plane that gates that API, then how to drive it with
Terraform. For the field-by-field template schema itself, see
[Defining a Template](../authoring/04-defining-a-template.md).

## The control plane is a separate, secret-gated surface

The registry serves two very different kinds of caller, and they authenticate
differently:

- **Agents** self-register and emit events using their **SPIFFE JWT-SVID** as a
  bearer. This is the workload-identity path the rest of the platform is built
  on — it is never behind a shared secret.
- **Operators and tooling** manage the *catalog* and the *consent broker* —
  creating, updating, disabling, and deleting templates. These **control-plane**
  endpoints sit behind a **shared-secret bearer**.

The mode is set by `CONTROL_PLANE_AUTH`:

| Value | Behaviour |
|-------|-----------|
| `shared-secret` *(default)* | Control-plane endpoints require the bearer token from the `control-plane-auth` Secret. |
| `none` | Control plane is open — no token required. Convenient for a throwaway local registry. |

`make bootstrap` generates the token **once** into a Kubernetes Secret named
`control-plane-auth` and wires the same value into the registry, the
orchestrator, IdentityServer, and `seed.sh` — so every in-cluster caller's token
matches by construction. Re-bootstrapping **reuses** the existing token, so
clients seeded mid-session (including a running Terraform provider) keep working.

To drive the control plane from **outside** the cluster, pull the token out of
the Secret:

```bash
export SPAWNLY_TOKEN=$(kubectl get secret control-plane-auth \
  -o jsonpath='{.data.token}' | base64 -d)
```

## Installing the provider

The provider module lives outside the repo's `go.work` workspace and builds
standalone:

```bash
cd terraform-provider-spawnly
make install                              # builds ./bin and writes dev.tfrc
export TF_CLI_CONFIG_FILE=$(pwd)/dev.tfrc
```

`make install` writes `dev.tfrc`, a CLI config with a `dev_overrides` block.
Under dev overrides you **skip `terraform init`** — just run `plan` / `apply`.

## Pointing it at a registry

Port-forward the in-cluster registry and point the provider at it. The provider
reads its endpoint and bearer from the environment (or from `endpoint` / `token`
in HCL):

```bash
kubectl port-forward svc/registry 18080:8080 &

export SPAWNLY_ENDPOINT=http://localhost:18080
export SPAWNLY_TOKEN=$(kubectl get secret control-plane-auth \
  -o jsonpath='{.data.token}' | base64 -d)
```

Against a registry running open (`CONTROL_PLANE_AUTH=none`) leave `SPAWNLY_TOKEN`
unset.

## What the provider exposes

| Kind | Name | Purpose |
|------|------|---------|
| Resource | `spawnly_agent_template` | A managed agent template — image, authority (`authz_template`, `oauth_scopes`), and a `delegation` block (allowed child types, grantable scopes, `max_depth`, per-child consent gating). |
| Data source | `spawnly_agent_template` | Read one existing template by name. |
| Data source | `spawnly_agent_templates` | List every template in the registry. |
| Data source | `spawnly_schema` | Read the active SpiceDB schema the registry is enforcing. |

```bash
cd examples/agent-template
terraform plan && terraform apply
terraform destroy
```

### The destroy caveat

The registry **refuses to delete an active template** — that would strand any
agent type mid-flight. `terraform destroy` handles this for you: it **disables
the template first, then deletes it**. The same two-step (disable → delete) is
what the control plane requires of any caller, Terraform or not.

## Reference and tests

The provider's own [README](../../terraform-provider-spawnly/README.md) has the
full provider configuration table, the richer delegation example, and the
generated per-resource docs. From the repo root, `make test-provider` runs the
whole gate — fmt/vet, unit + acceptance tests, and a parity check that the
provider can reproduce every seeded template — against an ephemeral registry it
stands up and tears down. No Kind cluster required; just `docker` and the
`terraform` CLI.

---

## The dashboard UI is a third, lighter writer over the same control plane

Admins can also manage templates from the dashboard — create, edit, duplicate,
disable, and delete — without leaving the browser. It drives the **same
control-plane API** this provider does, so it's handy for iteration and
incident response. Two caveats: the UI writes to the registry's **in-memory
store** (changes don't survive a restart unless committed to a
`template.json`), and it does **not** touch Terraform state, so the two can
drift (a `terraform apply` overwrites a UI-only edit; a UI delete shows as
drift on the next plan). Use the provider for the durable, reviewable record;
use the UI for quick experiments. See
[Managing agent types from the dashboard (admin)](dashboard-agent-types.md).
