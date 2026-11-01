# Security Policy

Spawnly is a proof-of-concept identity and authorisation platform for AI
agents. It is not production-hardened, but security reports are very welcome —
finding gaps in the identity, token-minting, and consent flows is exactly the
kind of feedback this project wants.

## Reporting a vulnerability

Please **do not open a public issue** for security problems. Instead, report
privately via GitHub's
[private vulnerability reporting](https://github.com/dbfr3qs/Spawnly/security/advisories/new)
("Security" tab → "Report a vulnerability").

Include what you can of: affected component (sidecar, registry, orchestrator,
Keycloak providers, operator, …), steps to reproduce, and the impact you
observed. You should get an acknowledgement within a week.

## Scope

In scope: everything in this repository — the Go services, the Keycloak SPI
providers, the operator, the SDKs, and the default deployment manifests.

Out of scope: third-party components (SPIRE, SpiceDB, Keycloak itself) except
where Spawnly configures or uses them insecurely, and the demo agents' use of
deliberately simplified sample APIs.
