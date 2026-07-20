package main

import (
	"context"
	"sort"
	"time"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// substratePusher is the narrow slice of the dispatch hub the reaper needs:
// enqueue one DOWN frame to a NAMED org's live channel. *substrateHub satisfies
// it. Keeping it this narrow makes own-org-only structural — the reaper can do
// nothing but push a frame to the org it was handed, and the hub only ever
// delivers to that org's own SVID-bound channel.
type substratePusher interface {
	push(org string, d channel.Down) bool
}

// substrateReaper drives control-plane→substrate teardown of substrate-placed
// agents. It is the home of the two Phase-4 code-review BLOCKERS:
//
//	B1  A live kill is now actually delivered. Before Phase 5 channel.NewKill had
//	    ZERO orchestrator callers, so a kill/dismiss/DELETE issued while the
//	    substrate was stably connected was never pushed. The reaper pushes
//	    channel.Kill down the target org's connected channel, so the substrate
//	    tears the pod/CR + org-scoped SPIRE entry down immediately.
//
//	B2  A substrate agent's DELETE/dismiss now marks the registry record TERMINAL
//	    (dismiss). A substrate record has NO AgentWorkload CR in the orchestrator's
//	    own cluster, so the platform DELETE path (which deletes CRs from the
//	    `default` namespace) was a silent no-op that returned 204 while leaving the
//	    record non-terminal — and because it was never marked killed/dismissed,
//	    reconcile-on-connect never tore it down either, so the agent ran forever
//	    holding its org-scoped SVID. Marking the record dismissed makes
//	    reconcile-on-connect a REAL backstop for an OFFLINE substrate, and the live
//	    push covers a CONNECTED one.
//
// Isolation: every kill is pushed to the record's OWN org — the org is taken from
// the server-authoritative registry record (AgentRecord.OrgID), NEVER a request
// field — and the hub only delivers to that org's own channel, so a teardown can
// never cross orgs. Kill commands flow CP→substrate only; nothing here acts on a
// substrate-reported status.
type substrateReaper struct {
	reg registry.Client
	hub substratePusher
	// settle is the pause between subtree settle-loop passes (a substrate chain
	// agent can self-spawn children mid-teardown, exactly like a platform chain).
	settle time.Duration
}

// isSubstratePlaced reports whether a record runs on a consumer substrate — its
// teardown must go over the channel + reconcile, not via a local CR delete.
func isSubstratePlaced(rec registry.AgentRecord) bool {
	return rec.Placement == registry.PlacementSubstrate
}

// pushKill delivers a live teardown for one substrate agent down its org's
// channel. Best-effort: an offline org (no channel) returns false and is fine —
// the caller has already (or will) mark the record terminal, so
// reconcile-on-connect is the backstop. delivered is for logging only; no control
// decision depends on it.
func (rp *substrateReaper) pushKill(org, agentID string) (delivered bool) {
	if rp == nil || rp.hub == nil {
		return false
	}
	return rp.hub.push(org, channel.NewKill(agentID))
}

// reapSubtree tears down a substrate-placed agent AND its whole descendant
// subtree from the control-plane side. For every node it (a) marks the registry
// record terminal via DismissAgent — so reconcile-on-connect tears it down even
// if the substrate is OFFLINE (B2) — and (b) pushes a channel.Kill for immediate
// LIVE teardown (B1). It settle-loops like the platform DELETE because a
// substrate chain agent can self-spawn children mid-teardown; it converges on the
// first pass that dismisses no new node.
//
// org is the record's OWN org: all subtree nodes share it (a child inherits its
// parent's org, and an active substrate takes ALL of an org's spawns), so every
// push is own-org. subtreeUserID is the manage-authorized user the caller already
// resolved (the registry re-checks it). Returns how many records were marked
// terminal and the ids whose dismiss errored (rendered 207 by the handler).
func (rp *substrateReaper) reapSubtree(ctx context.Context, rootID, org, subtreeUserID string) (dismissed int, failed []string) {
	const maxPasses = 8
	done := map[string]bool{}
	failedSet := map[string]bool{}

	tearDown := func(nid string) {
		if done[nid] {
			return
		}
		if err := rp.reg.DismissAgent(ctx, nid, subtreeUserID); err != nil {
			failedSet[nid] = true
			return
		}
		done[nid] = true
		delete(failedSet, nid)
		// Live teardown; an offline org just leaves the (now terminal) record for
		// reconcile-on-connect. Never blocks — push is non-blocking.
		rp.pushKill(org, nid)
	}

	for pass := 0; pass < maxPasses; pass++ {
		nodes, err := rp.reg.Subtree(ctx, rootID, subtreeUserID)
		if err != nil {
			break // registry read failed; the root fallback below still fires
		}
		if len(nodes) == 0 {
			break
		}
		before := len(done)
		for _, nid := range nodes {
			tearDown(nid)
		}
		if len(done) == before {
			break // converged: no new node dismissed this pass
		}
		if pass < maxPasses-1 && rp.settle > 0 {
			select {
			case <-ctx.Done():
				return len(done), sortedKeys(failedSet)
			case <-time.After(rp.settle):
			}
		}
	}

	// Root fallback: if the subtree read never yielded the root (a transient
	// registry blip), still tear down the node we were explicitly asked to delete —
	// it holds the org-scoped SVID that must not survive the DELETE.
	if !done[rootID] {
		tearDown(rootID)
	}

	return len(done), sortedKeys(failedSet)
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
