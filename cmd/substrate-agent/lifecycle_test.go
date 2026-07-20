package main

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// handleDown must dispatch spawn and kill through the same path a live frame
// takes (it now also takes opMu — proving that path still works end to end).
func TestHandleDown_DispatchesSpawnThenKill(t *testing.T) {
	store, entries := newFakeStore(), newFakeEntries()
	a := newTestAgent(store, entries, fakeLister{})

	a.handleDown(context.Background(), channel.NewSpawn(channel.SpawnCommand{AgentID: "agent-1", AgentType: "worker", UserID: "u"}))
	if _, ok := store.created["agent-1"]; !ok {
		t.Fatal("handleDown spawn did not create the CR")
	}
	a.handleDown(context.Background(), channel.NewKill("agent-1"))
	if _, ok := store.created["agent-1"]; ok {
		t.Fatal("handleDown kill did not tear the CR down")
	}
}

// The periodic-reconcile safety-net (Phase 5) runs reconcileOnce concurrently with
// live dispatch; opMu must serialize them so the shared store is never touched
// concurrently and neither deadlocks. Under `-race` this is the guard against the
// list-then-act reconcile racing a live handleSpawn.
func TestOpMu_ReconcileAndDispatchConcurrent(t *testing.T) {
	store, entries := newFakeStore(), newFakeEntries()
	lister := fakeLister{recs: []registry.AgentRecord{{AgentID: "r1", AgentType: "worker", OrgID: "acme", Status: "pending"}}}
	a := newTestAgent(store, entries, lister)

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = a.reconcileOnce(context.Background()) }()
		go func(n int) {
			defer wg.Done()
			a.handleDown(context.Background(), channel.NewSpawn(channel.SpawnCommand{
				AgentID: fmt.Sprintf("a-%d", n), AgentType: "worker", UserID: "u",
			}))
		}(i)
	}
	wg.Wait()

	// The reconcile-desired record and at least the dispatched agents converged.
	if _, ok := store.created["r1"]; !ok {
		t.Fatal("reconcile did not create the desired record under concurrency")
	}
}

// Revoke/resume parity on the substrate side: a REVOKED record still shouldRun
// (its pod is kept — revoke bites at the next token mint, not by deleting the
// pod), so reconcile never tears a revoked agent down. Kill/dismiss DO tear down.
// This is the substrate-side half of AC 7's revoke/resume parity.
func TestReconcile_RevokedKeepsRunning_TerminalTornDown(t *testing.T) {
	// Local CRs for two agents; desired says one is revoked (keep) and one dismissed.
	store := newFakeStore(localWorkload{AgentID: "revoked-1", AgentType: "worker"}, localWorkload{AgentID: "dismissed-1", AgentType: "worker"})
	entries := newFakeEntries()
	lister := fakeLister{recs: []registry.AgentRecord{
		{AgentID: "revoked-1", AgentType: "worker", OrgID: "acme", Status: "revoked"},
		{AgentID: "dismissed-1", AgentType: "worker", OrgID: "acme", Status: "active", Dismissed: true},
	}}
	a := newTestAgent(store, entries, lister)

	if err := a.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := store.created["revoked-1"]; !ok {
		t.Fatal("revoked agent was torn down — revoke must keep the pod (parity: mint-time enforcement)")
	}
	if _, ok := store.created["dismissed-1"]; ok {
		t.Fatal("dismissed agent was not torn down — a terminal record must be reaped")
	}
}
