---
title: Defining an Agent Template
description: The full AgentTemplate schema field by field, which component consumes each field, and the register/spawn lifecycle.
---

# Defining an Agent Template

> **Prerequisite:** [Anatomy of an Agent](00-anatomy.md), which introduces the
> template in passing. This guide owns the full contract.
>
> **Schema source of truth:** [`internal/registry/types.go`](../../internal/registry/types.go)
> (`AgentTemplate`). **Worked references:** the co-located `template.json` files
> (e.g. [`agents/travel-planner/template.json`](../../agents/travel-planner/template.json)),
> which [`scripts/seed.sh`](../../scripts/seed.sh) sweeps up.

An **agent template** is the registry's description of an agent *type*: the image
to run, how to run it, the relationships to grant it, and which children it may
delegate to. You register one template per `agentType`; every `POST /spawn` of
that type is materialised from it.

This guide is tutorial-first — we build a template from a blank file — followed
by a [reference appendix](#reference-appendix) you can return to.

---

## Tutorial: build a template from scratch

We'll define a template for a hypothetical `report-builder` agent and register
it. Start with the two things that name and describe the type:

```json
{
  "agentType": "report-builder",
  "version": "1.0.0",
  "status": "active",
  "meta": {
    "displayName": "Report Builder",
    "description": "Pulls data from a protected API and emits a report"
  }
}
```

`agentType` is the key everything else hangs off — it's what you pass to
`/spawn` and the registry's map key. `meta` is for humans and the dashboard.
(`version` and `status` are recorded but **not** enforced today — see
[status callouts](#status-callouts).)

### 1. Tell the platform how to run it — `runtimeSpec`

```json
  "runtimeSpec": {
    "image": "agent-report-builder:latest",
    "lifecycle": "short-lived",
    "resources": { "cpuLimits": "500m", "memoryLimits": "256Mi" },
    "envDefaults": { "LOG_LEVEL": "info" }
  }
```

- **`image`** — the container the operator runs. It must be loaded into Kind
  (`make kind-load`) and built from a [`Dockerfile`](../../Dockerfile) target.
- **`lifecycle`** — `short-lived` (default) or `long-lived`. This single field
  decides whether the operator creates a Service, whether pod-exit means
  "Completed", and whether the agent is **chattable** from the dashboard. See
  [the lifecycle switch](#the-lifecycle-switch) and
  [Chatting with a Long-Lived Agent](06-chat.md).
- **`supportsChat`** — set `true` on a long-lived agent that serves the
  `/agents/chat/:sessionId` endpoint, to offer the dashboard Chat button. See
  [Chatting with a Long-Lived Agent](06-chat.md).
- **`resources`** — CPU/memory limits applied to the agent container.
- **`envDefaults`** — extra env vars injected verbatim. Use these for
  agent-specific config (poll intervals, feature flags). See
  [env precedence](#environment-variable-precedence).

### 2. Grant it authority — `authzTemplate`

This declares the SpiceDB relationships written when an agent of this type
self-registers. The standard grant lets the agent call protected APIs for its
tenant:

```json
  "authzTemplate": {
    "spiceDbRelations": [
      { "resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}" }
    ]
  }
```

`{{tenant_id}}` and `{{agent_id}}` are expanded by the registry at registration
time ([`substitute()`](../../cmd/registry/main.go#L120)). What this grant *means*
— and how to design more of them — is the subject of
[05 — Defining Policy](05-defining-policy.md).

### Tenanted vs global agents

Whether an agent belongs to a tenant is **derived from the presence of a tenant
id at spawn**, not declared by a flag:

- **Tenanted** (the common case) — spawned with a `tenantId`. Its SVID is
  `spiffe://…/agent/<tenant>/<user>/<type>/<id>`, and any `{{tenant_id}}`
  relation above is written to SpiceDB.
- **Global / tenant-agnostic** — spawned **without** a `tenantId`. Its SVID drops
  the tenant/user segments (`spiffe://…/agent/<type>/<id>`), and the registry
  **skips** any relation that references `{{tenant_id}}` (so a template's tenant
  grant goes inert rather than writing a malformed `tenant:` tuple). Use this for
  agents that only call [tenant-agnostic resource servers](05-defining-policy.md#tenant-agnostic-resource-servers).

Because the tenant grant assumes a tenant, **set `requiresTenant: true` on any
template that has a `{{tenant_id}}` relation** — the orchestrator then rejects a
tenant-less spawn with `400` instead of letting it come up "global" with no
tenant grant (which would be silently denied by every tenant-checking API):

```json
  "requiresTenant": true
```

Leave it `false` (the default, or omit it) only for a genuinely tenant-agnostic
type whose `authzTemplate` has **no** `{{tenant_id}}` relation.

> ⚠️ This pairing is a convention, **not enforced**: nothing warns if a template
> has a `{{tenant_id}}` relation but omits `requiresTenant`. Keep them in sync by
> hand.

### 3. (Parent agents only) allow spawning / delegation — `delegation`

Omit this for an agent that never spawns children. **Any agent that spawns
children must list them in `allowedChildTypes`** — the orchestrator rejects a
spawn whose child type the parent doesn't list (deny-by-default), whether or not
authority is delegated. `grantableScopes`/`maxDepth` are only needed when the
parent also *delegates scopes*:

```json
  "delegation": {
    "allowedChildTypes": ["data-fetcher"],
    "grantableScopes": ["sample-api-b:read"],
    "maxDepth": 3
  }
```

Full treatment in [05 — Defining Policy](05-defining-policy.md#part-2--delegation).

### 4. Register it

The registry stores templates in memory. Save your template as a `template.json`
**next to your agent** (`agents/report-builder/template.json`; the real agents
follow the same pattern, e.g. `agents/chain-worker/template.json`) —
[`scripts/seed.sh`](../../scripts/seed.sh) discovers every co-located
`template.json` and POSTs it, so it survives a registry restart. Seed with:

```bash
make reseed        # runs scripts/seed.sh: sweeps up every template.json and POSTs it
```

To register a one-off without re-seeding everything, you can still POST directly:

```bash
# seed.sh port-forwards the registry to localhost:18080
curl -sf -X POST http://localhost:18080/v1/templates \
  -H 'Content-Type: application/json' \
  -d @agents/report-builder/template.json
```

Or manage the template declaratively with the
[`terraform-provider-spawnly`](../../terraform-provider-spawnly/README.md)
provider — `terraform apply` upserts it through the same control-plane API, and
`terraform destroy` disables-then-deletes it. See
[Config-as-code with Terraform](../operating/config-as-code.md).

### 5. Spawn and verify

```bash
curl -sf -X POST http://localhost:8080/spawn \
  -H 'Content-Type: application/json' \
  -d '{"agentType":"report-builder","tenantId":"tenant-1","userId":"user-1","task":"daily report"}'
# -> {"workloadName":"report-builder-xxxxx"}

curl -sf http://localhost:8080/v1/agents/report-builder-xxxxx/events | jq
```

That's the whole loop: define → register → spawn → observe.

---

## Reference appendix

### Full `AgentTemplate` schema

| Field | Type | Required | Notes |
|-------|------|----------|-------|
| `agentType` | string | ✅ | Unique type key; the registry's map key and the `/spawn` `agentType`. |
| `version` | string | — | Recorded only. Not used for selection ([status callouts](#status-callouts)). |
| `status` | string | — | `active` \| `deprecated`. Recorded only; not enforced. |
| `requiresTenant` | bool | — | When `true`, the orchestrator rejects a tenant-less spawn of this type. Default `false`. Set `true` whenever `authzTemplate` has a `{{tenant_id}}` relation. See [tenanted vs global](#tenanted-vs-global-agents). |
| `meta.displayName` | string | — | Shown on the dashboard. |
| `meta.description` | string | — | Human description. |
| `runtimeSpec.image` | string | ✅ | Container image (must be loaded into Kind). |
| `runtimeSpec.lifecycle` | string | — | `short-lived` (default) \| `long-lived`. |
| `runtimeSpec.supportsChat` | bool | — | `true` if the agent serves `/agents/chat/:sessionId`; gates the dashboard Chat button (long-lived only). See [Chatting with a Long-Lived Agent](06-chat.md). |
| `runtimeSpec.resources.cpuLimits` | string | — | K8s CPU limit, e.g. `500m`. |
| `runtimeSpec.resources.memoryLimits` | string | — | K8s memory limit, e.g. `256Mi`. |
| `runtimeSpec.envDefaults` | map | — | Extra env injected verbatim. |
| `authzTemplate.spiceDbRelations[]` | list | — | `{resource, relation, subject}` with `{{tenant_id}}`/`{{agent_id}}`. |
| `delegation.allowedChildTypes[]` | list | — | Child types this type may spawn/delegate to. |
| `delegation.grantableScopes[]` | list | — | Scope ceiling this type may pass down. |
| `delegation.maxDepth` | int | — | Max chain length / delegation depth; enforced at `/spawn` (chain length) and at token-exchange (delegation depth). `0` = unbounded check skipped. |

### Who consumes each field

The template is read by four components at different moments — a template is not
"used" in one place:

| Field(s) | Consumer | When |
|----------|----------|------|
| `requiresTenant` | **Orchestrator** | At `/spawn` — rejects the request with `400` if `true` and no `tenantId` was supplied. |
| `runtimeSpec.lifecycle` | **Orchestrator** | At `/spawn` — copied onto the `AgentWorkload` ([main.go:164](../../cmd/orchestrator/main.go#L164)). |
| `runtimeSpec.image`, `resources`, `envDefaults` | **Operator** | At pod build ([reconciler.go:224](../../internal/operator/reconciler.go#L224)). |
| `authzTemplate.spiceDbRelations` | **Registry** → SpiceDB | At agent self-registration (relations projected into SpiceDB). |
| `delegation.allowedChildTypes` | **Orchestrator** (via registry `/v1/spawn-policy`) | At `/spawn`, when a `parentId` is present (deny-by-default). |
| `delegation.maxDepth` | **Orchestrator** (via `/v1/spawn-policy`) + **IdentityServer** (via `/v1/delegation-policy`) | At `/spawn` (caps total chain length) and at token-exchange (caps delegation depth). |
| `delegation.grantableScopes` | **IdentityServer** (via registry `/v1/delegation-policy`) | At token-exchange. |

### Substitution tokens

Only two, expanded by the registry when an agent registers
([`substitute()`](../../cmd/registry/main.go#L120)):

| Token | Becomes |
|-------|---------|
| `{{tenant_id}}` | the spawning request's `tenantId` |
| `{{agent_id}}` | the agent's canonical id (`AGENT_ID`) |

For a [global agent](#tenanted-vs-global-agents) (no `tenantId`), any relation
referencing `{{tenant_id}}` is **skipped** at registration rather than written
with an empty tenant.

### The lifecycle switch

| `lifecycle` | Service created? | Pod exit `0` means | Used by |
|-------------|------------------|--------------------|---------|
| `short-lived` (default) | No | **Completed** ([reconciler.go:128](../../internal/operator/reconciler.go#L128)) | [Scenario 1](01-job-and-exit.md) |
| `long-lived` | Yes — `<AGENT_ID>-svc` ([reconciler.go:104](../../internal/operator/reconciler.go#L104)) | Nothing (stays Running until deleted) | [Scenario 2](02-loop-until-stopped.md), [child in Scenario 3](03-parent-and-child.md) |

### Environment-variable precedence

The operator builds the agent container's env list in this order
([`buildPod`](../../internal/operator/reconciler.go#L224)): platform-injected
vars (`AGENT_ID`, `TENANT_ID`, `REGISTRY_URL`, the API URLs, …) → `AI_*` from the
`ai-provider` Secret → your `envDefaults` → `TASK`.

> **Rule of thumb:** use `envDefaults` for **new** keys, not to override
> platform-injected ones. Don't name an `envDefaults` key the same as a reserved
> platform variable — relying on override behaviour for duplicate env names is a
> footgun. The full reserved list is the env table in
> [00 — Anatomy](00-anatomy.md#environment-the-operator-injects).

### The image / build contract

What the platform expects of a template's `image`:

- Built from a [`Dockerfile`](../../Dockerfile) target and loaded into Kind
  (`make kind-load`).
- Node agents bundle the compiled `@spawnly/sdk` from the `build-ts-sdk`
  stage (see the `weather-monitor` / `chain-worker` / `travel-planner` stages). A
  Go agent would instead build its own module in a dedicated stage (the Go SDK
  lives at [`sdks/go`](../../sdks/go)).
- **Long-lived** agents must listen on **port 8080** — that's the `targetPort`
  of the generated `<AGENT_ID>-svc` Service.

### Operating on templates

| Action | How |
|--------|-----|
| Register / update | `POST /v1/templates` (upsert by `agentType`); or `terraform apply` via the [provider](../operating/config-as-code.md). |
| List types | `GET /v1/templates`. |
| Persist across restarts | Drop a `template.json` next to your agent (`agents/<type>/template.json`); run `make reseed` ([`scripts/seed.sh`](../../scripts/seed.sh)). |
| Config-as-code | Manage templates declaratively with [`terraform-provider-spawnly`](../operating/config-as-code.md). |

### Status callouts

Honest notes about what is and isn't enforced today:

- ⚠️ **The registry store is in-memory** ([`newStore`](../../cmd/registry/main.go#L29)).
  Restarting the registry deletes every template and agent record. Always keep a
  `template.json` next to your agent and re-seed (`make reseed`) after redeploying
  the registry.
- ⚠️ **`version` is informational.** `getTemplate` keys on `agentType` only
  ([main.go:43](../../cmd/registry/main.go#L43)); there is no version selection.
  A second `POST` for the same `agentType` overwrites the first.
- ⚠️ **`status: deprecated` is not enforced.** Nothing filters deprecated
  templates from spawn. Treat it as a label, not a guardrail.

---

**Next:** [05 — Defining Policy](05-defining-policy.md) — what the `authzTemplate`
and `delegation` blocks actually authorise, and how that policy is enforced.
