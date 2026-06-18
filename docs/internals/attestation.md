---
title: Pluggable attestation
description: How attestation is abstracted so SPIFFE/SPIRE can be swapped for another mechanism like AWS STS behind a single ATTESTOR selector — the neutral contract, the five touchpoints, and the AgentId-consistency invariant.
---

# Pluggable attestation

Attestation is how a workload proves *who it is* before the platform mints it any
tokens. By default Spawnly uses **SPIFFE/SPIRE** (each pod gets a JWT-SVID via
the workload API), but the attestation seam is pluggable so SPIRE can be
replaced — e.g. with **AWS IRSA / STS**, where a pod's projected ServiceAccount
token is the credential.

This doc describes the seam. The access tokens that resource servers actually
check are minted by the IdentityServer and are **already attestor-neutral**, so
nothing below touches resource servers.

## The neutral contract

Every attestor, on the control-plane side, reduces a presented credential to one
shape:

| Field | Meaning |
|---|---|
| `AgentId` | Registry-facing identifier (primary key of the agent record). SPIFFE: last path segment of the SVID URI. |
| `Subject` | Raw verified identity string (the SPIFFE URI today). Lands in the token's `act.sub`. |
| `Issuer` | Which verifier produced it (`spiffe-svid`, `aws-sts`, …) — audit + mixed deployments. |

Realized as Go `registrant.Identity` and C# `AgentIdentity` — kept in sync on
purpose (see the invariant below).

## The five touchpoints

| # | Stage | Component | Abstraction |
|---|---|---|---|
| 1 | Identity delivered into the pod | operator | `operator.IdentityInjector` (`SpiffeInjector`) |
| 2 | Agent fetches its credential | sidecar | `attestor.Source` (`SpiffeSource`) |
| 3 | Agent presents it to mint tokens | sidecar | OAuth `client_assertion` / `actor_token`; `Credential.AssertionType` |
| 4 | Control plane verifies → mints | identityserver | `IAgentCredentialVerifier` (`SpireCredentialVerifier`) |
| 5 | Registry verifies self-registration | registry | `registrant.Verifier` (`SpiffeVerifier` / `OIDCVerifier` / `MTLSVerifier`) |

All five are selected by the single env var **`ATTESTOR`** (default `spiffe`).
The registry additionally honors `REGISTRANT_VERIFIER` as an explicit override.

## The consistency invariant

> The `AgentId` derived from a credential MUST be identical on both the registry
> side (touchpoint 5) and the IdentityServer side (touchpoint 4).

If they disagree, a minted token's `sub`/`act` won't match the registry record
and every downstream authorization check fails. When adding an attestor, make
both verifiers derive `AgentId` the same way.

## Adding a new attestor (e.g. AWS IRSA)

1. **Sidecar** — implement `attestor.Source` (read the projected SA token from
   `AWS_WEB_IDENTITY_TOKEN_FILE`, return it as a `jwt-bearer` credential); add a
   case in the sidecar's `ATTESTOR` switch.
2. **IdentityServer** — implement `IAgentCredentialVerifier` (validate the SA
   token against the cluster OIDC JWKS, derive `AgentId` from a claim); add a
   case in `Program.cs`.
3. **Operator** — implement `IdentityInjector` (projected SA-token volume +
   `AWS_ROLE_ARN`/`AWS_WEB_IDENTITY_TOKEN_FILE` env + the IRSA SA); add a case in
   the operator's `ATTESTOR` switch.
4. **Registry** — point `ATTESTOR=aws-sts` at the existing `OIDCVerifier` with
   the same `AgentId` claim as step 2 (already mapped in registry `main.go`).
5. **Deploy** — ship `deploy/aws/` manifests; skip the SPIRE install in
   `bootstrap.sh` when `ATTESTOR=aws-sts`.

Steps 1–4 are independent files behind the selector; SPIRE remains the default
and untouched.
