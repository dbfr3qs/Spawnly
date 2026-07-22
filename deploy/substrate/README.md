# Substrate bundle (local sim overlay)

The **remote-substrate** local-simulation bundle: the kustomize overlay a
customer would install into their own Kubernetes cluster (a "substrate") to run
Spawnly agents on **their** compute while the Spawnly control plane stays the
authority for identity, consent, tenancy, and lifecycle. See
`plans/remote-substrate/spec.md` / `plan.md` for the full design, and
`plans/remote-substrate/runbook.md` for the two-real-kind-cluster walkthrough +
the path to a real `spawnly.run` edge.

This overlay is a **local sim**: it runs in a dedicated namespace
(`substrate-sim`) of the *same* kind cluster as the platform, so the "hosted
SPIRE server" is the platform's own SPIRE server. Nothing here is a real remote
deployment — the AWS public edge (SPIRE gRPC exposure, per-org opaque endpoints,
the real Spawnly CA) is the companion `spawnly-infra` track and is **out of this
repo's done-bar** (`plans/remote-substrate/infra-edge.md`).

## What it deploys

- `namespace.yaml` — the `substrate-sim` namespace.
- `spire-agent.yaml` — a SPIRE **agent** DaemonSet that attests to the hosted
  server via **x509pop** using a CP-issued, **org-bound** leaf (there is no SPIRE
  server, CA, or bundle management on the substrate side).
- `spiffe-csi-driver.yaml` — the SPIFFE CSI driver that mounts workload SVIDs.
- `operator.yaml` — the reused `internal/operator` reconciler, watching
  `substrate-sim`, that materializes dispatched `AgentWorkload` CRs into pods.
- `substrate-agent.yaml` — `cmd/substrate-agent`: holds the outbound dispatch
  channel to the orchestrator (authenticated by its org-scoped SVID), reconciles
  desired-vs-actual on connect, and requests org-scoped SPIRE entries via the
  CP-mediated Entry API.
- `networkpolicy.yaml` — restricts the sim namespace's egress.

**Not** in this overlay (applied by the setup scripts, since they carry
generated material or copy cross-namespace state): the dev CA Secret, the
org-bound identity Secret, and the copied SPIRE trust bundle.

## Running it (local sim / e2e)

The first-class harness stands up a **two-org** sim — org A with a live
substrate, org B with credentials only for the isolation negatives:

```
make bootstrap            # platform cluster
make e2e-anchor-setup     # demo orgs (acme, globex) + fake-sts
make substrate-e2e-setup  # dev CA + x509pop + this overlay + Entry-API socket + org-B creds
```

Then prove it:

```
./scripts/substrate-verify.sh acme        # identity loop: x509pop attest + org SVID (AC 1/4)
./scripts/substrate-isolation.sh acme globex   # two-org isolation matrix (AC 1/4/5)
cd e2e && npx playwright test substrate    # the substrate Playwright specs
```

Tear down with `make substrate-down` (add `SUBSTRATE_UNWIRE=1` to also unwire the
Entry-API delegated-admin socket). Individual pieces: `make substrate-ca`,
`make substrate-setup ORG=<org>`, `make substrate-wire-entry-api`,
`make substrate-isolation`.

## Security posture (local sim)

- **No SPIRE server / CA / AWS creds** live on the substrate side — only a SPIRE
  agent with a CP-issued org-bound leaf.
- The **dev CA is generated at bootstrap** into the `substrate-dev-ca` Secret and
  is **never committed or baked into an image**. The real Spawnly CA is the infra
  track.
- Isolation is **path + entry-authz + dual-verifier**, not boundary-based: node
  attestation binds the node→org cryptographically (x509pop CN), registration
  entries are created only under the caller's own org namespace via the
  CP-mediated Entry API, and both verifiers enforce trust-domain **and**
  org-from-path == record-org. Server-authoritative `orgId` throughout.
- The Entry-API delegated-admin socket (`make substrate-wire-entry-api`) shares
  the SPIRE server's admin socket over a hostPath — **local-sim only**. A real
  deploy never exposes that socket; the infra edge fronts the server's entry gRPC
  admin API behind mTLS, upgrading `execEntryBroker` to a gRPC broker behind the
  same `substrate.EntryBroker` seam with no authz change.
- `scripts/substrate-setup.sh` copies the platform's `control-plane-auth` Secret
  into `substrate-sim` for the sim's registry reads — a **SIM-ONLY** shortcut,
  documented inline as never-ship. A genuine remote substrate reaches only the
  public edge over its org-scoped SVID.
