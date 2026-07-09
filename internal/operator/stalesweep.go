// internal/operator/stalesweep.go
package operator

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
)

// staleSweepStatuses are the registry statuses that MUST be backed by a live
// AgentWorkload to be legitimate. A record in one of these states with no
// matching CR is an orphan left over from a cluster rebuild (the durable
// registry survives; the workloads do not) and is swept to `failed`.
//
// Deliberately excluded:
//   - pending: the orchestrator preregisters the record (status `pending`)
//     BEFORE the operator creates its AgentWorkload, so a `pending` record with
//     no workload is a normal in-flight spawn, not an orphan. Sweeping it would
//     race and kill live spawns.
//   - completed/failed: already terminal — nothing to do, and re-failing would
//     churn SpiceDB cleanup and emit noise.
//   - dismissed records: excluded by ListAgents itself (it drops Dismissed).
var staleSweepStatuses = map[string]bool{
	"active":           true,
	"awaiting-consent": true,
	"revoked":          true,
}

// StaleSweeper reconciles the durable registry against the cluster: agent
// records whose backing AgentWorkload no longer exists are marked `failed` via
// the existing control-plane PATCH surface, so a rebuilt cluster never presents
// phantom-`active` agents in the dashboard.
//
// The registry stays k8s-blind by design — this reconciliation is entirely
// operator-driven over the registry's HTTP API (ListAgents + Fail); the
// registry gains no cluster awareness.
type StaleSweeper struct {
	// Registry is the control-plane-authenticated client used to list agents and
	// PATCH orphans to `failed` (which triggers the registry's standard terminal
	// SpiceDB relation cleanup — history and events are preserved).
	Registry registry.Client
	// Events posts the audit trail for each sweep; best-effort (may be nil).
	Events events.Client
	// Reader lists AgentWorkload CRs. Backed by the manager's cache, so it MUST
	// only be used after cache sync (see the Runnable wiring in cmd/operator).
	Reader client.Reader
	// Namespace is the agent namespace holding the AgentWorkload CRs.
	Namespace string
}

// Sweep marks every orphaned non-terminal agent record `failed`. trigger names
// what fired this sweep ("startup" | "retry" | "resync") and is threaded into
// the audit event so a resync-driven kill isn't misattributed to boot.
//
// Fail-safe: if EITHER list (agents or AgentWorkloads) errors, the whole sweep
// aborts without patching anything. Marking agents failed off a partial or
// empty read would terminate live agents — an errored/empty workload list would
// make every live agent look orphaned — so we never act on an unreliable read.
//
// LOAD-BEARING ORDERING — agents FIRST, workloads SECOND. Fail is terminal
// (SpiceDB relations cleaned, re-registration refused) and the registry has no
// server-side transition guard (the status PATCH overwrites unconditionally),
// so this client-side ordering is the ONLY defense against destroying a live
// agent. If workloads were snapshotted first, a spawn completing during a slow
// ListAgents round-trip (the registry HTTP client allows up to 30s) could show
// up `active` in the agents list yet be missing from the EARLIER workload
// snapshot — and the `pending` exclusion would not save it, because its sidecar
// has already flipped the record past pending. Snapshotting agents first closes
// the race: any record already active/awaiting-consent at the agents-snapshot
// instant had its AgentWorkload created strictly earlier — the orchestrator
// creates the CR before preregistering the record, and either way the sidecar
// can only flip a record past `pending` from inside a pod the operator created
// FROM that CR, so CR-existence strictly precedes every swept status — meaning
// the LATER workload List cannot miss its CR. Do not "tidy" this back to
// workloads-first.
func (s *StaleSweeper) Sweep(ctx context.Context, trigger string) error {
	logger := log.FromContext(ctx).WithName("stalesweep").WithValues("trigger", trigger)

	agents, err := s.Registry.ListAgents(ctx)
	if err != nil {
		// Fail-safe: never patch off an errored registry read.
		logger.Error(err, "listing agents; skipping sweep")
		return err
	}

	var awList agentv1alpha1.AgentWorkloadList
	if err := s.Reader.List(ctx, &awList, client.InNamespace(s.Namespace)); err != nil {
		// Fail-safe: cannot trust the workload set → do not patch anything.
		logger.Error(err, "listing AgentWorkloads; skipping sweep")
		return err
	}
	// AgentWorkload CR name == registry AgentRecord.AgentID, so the name-set is
	// the set of agent ids that still have a live workload.
	live := make(map[string]struct{}, len(awList.Items))
	for i := range awList.Items {
		live[awList.Items[i].Name] = struct{}{}
	}

	var swept int
	for _, a := range agents {
		if a.Placement == registry.PlacementRemote {
			// Remote (consumer-hosted) agents have NO AgentWorkload CR by design —
			// they run on the consumer's own compute, not in this cluster (Phase 10).
			// The sweep reaps records with no backing workload, so without this skip
			// EVERY remote agent would look orphaned and be terminally killed on the
			// operator's first startup sweep. Exclude them BEFORE the live/CR check.
			// Placement is server-set at preregistration and immutable, so a sidecar
			// cannot forge it to make a k8s agent immortal.
			continue
		}
		if !staleSweepStatuses[a.Status] {
			continue // terminal, pending, or otherwise not workload-backed
		}
		if _, ok := live[a.AgentID]; ok {
			continue // has a live workload — legitimate
		}
		// Orphan: a non-terminal record with no backing workload. Fail it via the
		// existing control-plane PATCH; the registry then performs the standard
		// terminal SpiceDB relation cleanup, exactly as for a normally-failed
		// agent. A per-agent Fail error is logged and skipped (best-effort) rather
		// than aborting the whole sweep — the next resync retries it.
		if err := s.Registry.Fail(ctx, a.AgentID); err != nil {
			logger.Error(err, "marking stale agent failed", "agentId", a.AgentID, "wasStatus", a.Status)
			continue
		}
		swept++
		logger.Info("marked stale agent failed", "agentId", a.AgentID, "wasStatus", a.Status)
		if s.Events != nil {
			// Best-effort audit trail; a failed post never blocks the sweep.
			_ = s.Events.PostEvent(ctx, a.AgentID, events.Event{
				Source: events.SourceOperator,
				Type:   "reconcile.stale",
				Payload: mustMarshal(map[string]string{
					"reason":    fmt.Sprintf("no workload found — marked failed by %s reconciliation", trigger),
					"wasStatus": a.Status,
				}),
			})
		}
	}
	if swept > 0 {
		logger.Info("stale-agent sweep complete", "markedFailed", swept, "scanned", len(agents))
	}
	return nil
}
