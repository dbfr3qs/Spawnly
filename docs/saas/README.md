# Productionizing the registry as a SaaS-ready reference architecture

These plans take the Spawnly **registry** (`cmd/registry`, `internal/registry`)
from an in-memory, SPIRE-and-`tenant`-hardcoded POC toward a *SaaS-ready
reference architecture*: its couplings to SpiceDB, the IdP, and the K8s runtime
become pluggable behind interfaces with opinionated defaults, so a consumer can
adopt it in their own environment with minimal custom code.

**Scope note:** "SaaS-ready" means pluggable adapters + sane defaults, *not* a
hosted product — no billing, signup, control plane, or per-customer data
isolation. That's a deliberately larger effort left for later (see Phase 4's
out-of-scope).

## The product boundary

The registry bundles four separable concerns. The first two (plus the revoke
cascade) are the differentiated core; the rest are adapters:

| Concern | Role | Plan |
|---|---|---|
| Lineage records + delegation policy | **Core** | (existing) |
| Authz materialization (agent → SpiceDB tuples) | Adapter → SpiceDB | Phases 1, 2, 5a |
| Caller authentication (who may register) | Adapter → IdP | Phase 3 |
| Consent store | Adapter → IdP, becoming core | Phase 5b |
| Persistence | Adapter → store backend | Phase 4 |

Token minting (`sub=user`, `act=agent`) stays in the consumer's IdP and is *not*
in the registry's scope — only the consent REST contract and the `act`-claim
convention are.

## Phases

Ordered by leverage and dependency. Each doc has goal, current-state evidence
(`file:line`), target design, step-by-step, files to touch, acceptance
criteria, and cross-phase interactions.

1. **[Fix the SpiceDB delete leak](phase-1-spicedb-delete-leak.md)** —
   `DeleteAgentRelationships` hardcodes `ResourceType: "tenant"`, so any consumer
   schema that doesn't use a `tenant` resource silently can't clean up on
   revoke/complete. One-function interface change; unblocks every custom schema.
2. **[Registry-owned schema & template validation](phase-2-schema-ownership-and-validation.md)** —
   the SpiceDB schema is a hardcoded `const` in the *orchestrator* while the
   registry writes the conforming tuples (split ownership); and a template whose
   relations don't match the schema fails silently. Move schema ownership into
   the registry (default + override) and validate templates on upload.
3. **[Pluggable registration auth](phase-3-pluggable-registration-auth.md)** —
   registration hardwires SPIFFE/SPIRE SVID validation. Introduce a `Registrant`
   verifier (SPIFFE / OIDC JWT / mTLS) so non-SPIRE consumers can register.
4. **[Persistence: `Store` interface (+ DynamoDB reference design)](phase-4-persistence-store-interface.md)** —
   the in-memory store resets on restart. Extract a `Store` interface, keep
   in-memory as the portable default, and *document* (don't build) a DynamoDB
   single-table design as the reference AWS prod impl. No relational DB needed —
   all access patterns are key-value/hierarchical.
5. **[SpiceDB-native revoke & registry-native consent broker](phase-5-native-revoke-and-consent-broker.md)** —
   (a) replace write-all/delete-all-and-re-derive revoke with a single `enabled`
   status relationship (`permission work_on = agent & agent->enabled`); revoke =
   delete one tuple. (b) Make the registry broker its own consent
   approve/deny lifecycle so it no longer *requires* a CIBA-capable IdP; CIBA
   becomes one optional driver.

## Dependency order

```
Phase 1  (independent — do first)
Phase 3  (independent)
Phase 4  (independent; anticipates consumer-tenancy via list-key GSI)
Phase 2  (owns the default schema bundle)
   └── Phase 5a  (extends the default schema with `enabled`; composes with Phase 1's delete path)
Phase 5b (consent broker; touches IdentityServer integration — keep REST contract working)
```

Phases 1, 3, 4 have no inter-dependencies and can proceed in parallel. Phase 5a
depends on Phase 2 (registry owning + extending the schema) and composes with
Phase 1 (the generalized delete path). Phase 5b is largely independent but
should land after the `Store` interface (Phase 4) so the pending→approved/denied
state has a home.

## Key decisions already made

- **Reference arch, not hosted SaaS** — pluggable adapters + defaults only.
- **No relational DB** — Dynamo is documented as the reference AWS impl; the
  portable default stays in-memory behind a `Store` interface (Phase 4).
- **Registry-native consent broker, CIBA optional** (Phase 5b).
- **Status relation over caveat** for native revoke (Phase 5a).
