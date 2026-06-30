---
title: Managing agent types from the dashboard (admin)
description: How an admin uses the dashboard UI to create, edit, disable, and delete agent templates — who counts as an admin, what the UI can do, and the drift and in-memory-persistence caveats.
---

# Managing agent types from the dashboard (admin)

> **Prerequisite:** [Defining an Agent Template](../authoring/04-defining-a-template.md)
> for the full template schema, and [Config-as-code with Terraform](config-as-code.md)
> for the control plane the UI drives.

The dashboard ships an **Agent Types** view, visible only to admins, that lets
you manage the agent-template catalog without leaving the browser: create a new
type, edit an existing one, duplicate it as a starting point, and disable or
delete it. It talks to the same control-plane API that
[`scripts/seed.sh`](../../scripts/seed.sh) and the
[`terraform-provider-spawnly`](../../terraform-provider-spawnly/README.md)
provider use — it is a **convenience surface over the same backend**, not a
separate store.

This page covers who an admin is, what the UI can do, and the two caveats every
operator should internalise before relying on it.

## Who is an admin

Admin status is an IdentityServer **role claim**, not something the dashboard or
orchestrator decide. A user is an admin when their token carries:

```
role = admin
```

- In IdentityServer, the demo admin user (`alice`/`alice`) is seeded with this
  claim in [`identityserver/TestUsers.cs`](../../identityserver/TestUsers.cs).
  The `role` claim rides into the **id token** (so the dashboard BFF can read
  it and toggle the UI) and into the **orchestrator access token** (so the
  orchestrator can enforce it) — see
  [`identityserver/Config.cs`](../../identityserver/Config.cs) (`roles`
  IdentityResource + `UserClaims = { "role" }` on the orchestrator
  `ApiResource`).
- A second, non-admin `viewer` user exists for the demo's deny-path; it is
  seeded only by [`scripts/bootstrap.sh`](../../scripts/bootstrap.sh) for local
  and e2e use, and is **absent** from the public deployment (fail-closed).

The UI's **Agent Types** nav entry is shown only when `/api/me reports
`isAdmin: true`. That is **cosmetic convenience** — the real boundary is
server-side (see below). A tampered client that unhides the nav still gets `403`
from every management route.

## What the UI can do

Open **Agent Types** from the header (admins only). You can:

- **List** all templates — active **and** disabled — with status, tenant
  requirement, and a delegation summary. (The public spawn dropdown only ever
  sees active types.)
- **Create** a type via a guided form: the human-readable fields (image,
  lifecycle, resources, env defaults, OAuth scopes, requires-tenant, allowed
  children) plus **raw JSON editors** for the structured
  `authzTemplate` and `delegation` blocks. Client-side validation catches a
  bad `agentType` or unparseable JSON before the request leaves the browser;
  server validation errors (400) surface **inline** in the form, not just as a
  toast.
- **Edit** a type — `agentType` is the immutable key and is read-only when
  editing; saving replaces the template in place (an upsert by key, so it never
  leaves a stray duplicate).
- **Duplicate** a type as a starting point — pre-fills the create form from an
  existing template with `agentType` cleared, so you can clone-and-tweak.
- **Disable / enable** a type with `PATCH` — disabling hides it from the spawn
  dropdown and blocks new spawns.
- **Delete** a type — available **only when it is disabled**; deleting an active
  type is blocked at the UI (disabled button), at the JS layer (a guard), and at
  the server (`409 Conflict`). This mirrors the Terraform provider's
  disable-then-delete cascade.
- **Inspect** a type's full detail, including the raw JSON.

## The admin gate is server-side, in two places

UI hiding is cosmetic. Every management route is enforced at **both** tiers, so
a non-admin cannot manage templates even by calling the routes directly:

| Route | Gate |
|-------|------|
| `GET /api/admin/templates` (full list, incl. disabled) | `requireAdmin` at the BFF **and** the orchestrator |
| `POST /api/templates` (create/replace) | `requireAdmin` at both tiers |
| `PATCH /api/templates/{type}` (status) | `requireAdmin` at both tiers |
| `DELETE /api/templates/{type}` | `requireAdmin` at both tiers |
| `GET /api/templates` (active-only spawn list) | ordinary `require` (any logged-in user) |

The BFF checks the session's `role` claim; the orchestrator checks the access
token's own `role` claim independently. A non-admin gets `403` and is never
forwarded to the registry.

## Caveats — read these before you rely on the UI

> ⚠️ **In-memory store.** The registry holds templates in memory. A type you
> create or edit **does not survive a registry restart** unless it is also
> committed to `agents/*/template.json` in the repo (swept up by
> `scripts/seed.sh`) or managed by the Terraform provider. The UI shows this
> caveat inline. Use the UI to iterate quickly, then promote a keeper to a
> committed `template.json` — or it will silently disappear on the next
> reseed/restart.

> ⚠️ **Drift.** The UI writes directly to the registry's in-memory store; it
> does **not** touch git or Terraform state. If you also manage a type with the
> Terraform provider, the two can diverge: a `terraform apply` will overwrite a
> UI-only edit, and a UI delete will show up as Terraform drift on the next
> plan. Treat the UI as one writer among several, not as the source of truth.
> The committed `template.json` files (and Terraform) are the durable record.

## This complements, it does not replace, config-as-code

The dashboard UI is the fastest path to experiment and to make a quick
operational change. For anything you want to keep — especially across restarts
and across the team — prefer the durable writers:

- **`agents/*/template.json` + `scripts/seed.sh`** for the canonical catalog
  ([Defining an Agent Template](../authoring/04-defining-a-template.md)).
- **The Terraform provider** for declarative, reviewable, drift-detectable
  management ([Config-as-code with Terraform](config-as-code.md)).

The UI is a third, lighter option for the same control plane — handy for
iteration and incident response, not for the durable record.
