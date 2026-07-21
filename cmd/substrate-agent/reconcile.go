package main

import (
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spiffe"
)

// localWorkload is the substrate-agent's view of ONE local AgentWorkload CR it
// manages: enough to tear it down (agentID = CR/record key) and to rebuild the
// org-scoped SPIRE entry id for teardown (agentType is a path segment).
type localWorkload struct {
	AgentID   string
	AgentType string
}

// shouldRun reports whether a substrate-placed registry record describes an agent
// that SHOULD have a live local CR. Terminal states (completed/failed) and a
// dismissed record are torn down; pending (preregistered, not yet materialized),
// active (running), and revoked (still running — revoke bites at mint time, not
// by deleting the pod) all keep their CR.
func shouldRun(rec registry.AgentRecord) bool {
	if rec.Dismissed {
		return false
	}
	switch rec.Status {
	case "completed", "failed":
		return false
	default:
		return true
	}
}

// reconcile computes the desired-vs-actual delta for the substrate on
// (re)connect. desired is the caller org's substrate-placed registry records
// (the authoritative desired state pulled over the org-scoped list); actual is
// the substrate's current local CRs. It returns the records to CREATE (a
// should-run, non-revoked record with no local CR) and the local CRs to DELETE
// (no matching should-run record — the record was killed, dismissed, or removed
// while the substrate was offline). Pure and deterministic, so it is unit-tested
// directly.
func reconcile(desired []registry.AgentRecord, actual []localWorkload) (toCreate []registry.AgentRecord, toDelete []localWorkload) {
	want := make(map[string]registry.AgentRecord, len(desired))
	for _, rec := range desired {
		if shouldRun(rec) {
			want[rec.AgentID] = rec
		}
	}
	have := make(map[string]struct{}, len(actual))
	for _, w := range actual {
		have[w.AgentID] = struct{}{}
	}
	for id, rec := range want {
		if _, ok := have[id]; ok {
			continue
		}
		// Keep-but-never-create: a REVOKED record keeps an existing CR (revoke bites
		// at mint time, not by deleting the pod) but must never be MATERIALIZED from
		// scratch. Revoking an agent that was still `pending` — which the control
		// plane now permits, so an in-flight spawn can actually be severed — would
		// otherwise leave the substrate creating its pod and authoring its SPIRE
		// entry on the next reconcile, spending consumer compute and issuing an
		// identity for an agent whose owner explicitly revoked it. It could still
		// never mint (a revoked record refuses self-registration), but the pod has no
		// business existing. A later resume flips the record back to `active`, and
		// the reconcile after that materializes it normally.
		if rec.Status == "revoked" {
			continue
		}
		toCreate = append(toCreate, rec)
	}
	for _, w := range actual {
		if _, ok := want[w.AgentID]; !ok {
			toDelete = append(toDelete, w)
		}
	}
	return toCreate, toDelete
}

// ensureEntryFor returns the should-run records that ALREADY have a local CR —
// the complement of reconcile's toCreate within the desired set. These are the
// workloads whose org-scoped SPIRE entry must be RE-ASSERTED on (re)connect: a
// connect is also a rotate, and the CP sweeps every entry under the org's previous
// x509pop node, so a live workload's identity is gone while its CR looks healthy.
// Pure and deterministic, like reconcile, so the rotate-survival rule is unit-testable.
func ensureEntryFor(desired []registry.AgentRecord, actual []localWorkload) []registry.AgentRecord {
	have := make(map[string]struct{}, len(actual))
	for _, w := range actual {
		have[w.AgentID] = struct{}{}
	}
	var out []registry.AgentRecord
	for _, rec := range desired {
		if !shouldRun(rec) {
			continue
		}
		if _, ok := have[rec.AgentID]; ok {
			out = append(out, rec)
		}
	}
	return out
}

// workloadSpiffeID builds the ORG-SCOPED workload SVID id the substrate registers
// via the CP Entry API. It is the single funnel both create and teardown use so
// the id round-trips through spiffe.ParsePath to the same {org, agentID}
// (path.Base == agentID == the record key). Shape:
//
//	spiffe://<td>/org/{org}/agent/{agentType}/{agentID}
//
// mirroring the platform's global ClusterSPIFFEID template but org-scoped, so the
// dual verifiers bind the resulting SVID to the record's org.
func workloadSpiffeID(trustDomain, org, agentType, agentID string) (string, error) {
	return spiffe.BuildOrgAgentPath(trustDomain, org, agentType, agentID)
}

// workloadSelectors are the SPIRE k8s workload selectors the entry binds:
// namespace + the deterministic pod name (CR name + "-pod", per the operator's
// buildPod) + service account. The Entry API requires all three; pod-name makes
// the entry unique to this workload even under the shared default SA.
func workloadSelectors(namespace, agentID, serviceAccount string) []string {
	if serviceAccount == "" {
		serviceAccount = "default"
	}
	return []string{
		"k8s:ns:" + namespace,
		"k8s:pod-name:" + agentID + "-pod",
		"k8s:sa:" + serviceAccount,
	}
}
