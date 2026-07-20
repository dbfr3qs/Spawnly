package main

import (
	"context"
	"log"
	"sync"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// agent is the transport-independent core of the substrate-agent: it turns
// dispatch commands and reconcile deltas into local CR + org-scoped SPIRE-entry
// operations, and reports status UP. The WebSocket plumbing (connect, backoff,
// read/write) lives in connect.go and only ever calls these methods — so the
// behavior is unit-testable with fakes, no cluster or socket needed.
//
// Every entry it authors is under its OWN org (a.org, derived from its verified
// SVID at startup) parented to its OWN x509pop node (a.nodeID, computed from its
// mounted leaf). It NEVER trusts SpawnCommand.OrgID — a compromised CP view or a
// malformed frame cannot make it author cross-org identity, and the Entry API
// re-checks org server-side regardless.
type agent struct {
	trustDomain    string
	org            string // from the substrate-agent's own SVID — authoritative
	namespace      string
	nodeID         string // the org's x509pop node (entry parent)
	serviceAccount string

	store   workloadStore
	entries entryClient
	lister  desiredLister

	// send reports an UP frame (status/heartbeat) to the CP. The transport swaps it
	// per connection (set on connect, cleared to nil on disconnect) while the
	// writer/heartbeat/reconcile goroutines call report() concurrently, so access is
	// mutex-guarded — otherwise the clear races those reads (would trip -race). A
	// nil send drops reports (used by unit tests that assert side effects only).
	mu   sync.Mutex
	send func(channel.Up)
}

// setSend installs (or clears, with nil) the per-connection UP-frame sink.
func (a *agent) setSend(f func(channel.Up)) {
	a.mu.Lock()
	a.send = f
	a.mu.Unlock()
}

func (a *agent) report(u channel.Up) {
	a.mu.Lock()
	f := a.send
	a.mu.Unlock()
	if f != nil {
		f(u)
	}
}

// handleDown dispatches one CP→substrate frame.
func (a *agent) handleDown(ctx context.Context, d channel.Down) {
	switch d.Type {
	case channel.TypeSpawn:
		a.handleSpawn(ctx, *d.Spawn)
	case channel.TypeKill:
		a.handleKill(ctx, d.Kill.AgentID)
	}
}

// handleSpawn materializes a dispatched agent: create the local CR, then request
// its org-scoped SPIRE entry. Org is ALWAYS a.org (never cmd.OrgID). On any error
// it reports phase=failed; the record stays pending and reconcile-on-reconnect
// retries, so a transient failure is self-healing.
func (a *agent) handleSpawn(ctx context.Context, cmd channel.SpawnCommand) {
	a.report(channel.NewStatus(cmd.AgentID, channel.PhaseCreating, ""))
	rec := registry.AgentRecord{
		AgentID:         cmd.AgentID,
		AgentType:       cmd.AgentType,
		UserID:          cmd.UserID,
		TenantID:        cmd.TenantID,
		OrgID:           a.org,
		Lifecycle:       cmd.Lifecycle,
		ParentID:        cmd.ParentID,
		ConsentRequired: cmd.ConsentRequired,
	}
	if err := a.materialize(ctx, rec, cmd.Task); err != nil {
		log.Printf("substrate-agent: spawn %s failed: %v", cmd.AgentID, err)
		a.report(channel.NewStatus(cmd.AgentID, channel.PhaseFailed, err.Error()))
		return
	}
	a.report(channel.NewStatus(cmd.AgentID, channel.PhaseCreated, ""))
}

// materialize creates the CR then authors the entry. Order matters: the CR (hence
// the pod) can exist before the entry, and SPIRE will issue the SVID once the
// entry lands; authoring the entry first would risk a dangling entry if CR create
// fails.
func (a *agent) materialize(ctx context.Context, rec registry.AgentRecord, task string) error {
	if err := a.store.Create(ctx, rec, task); err != nil {
		return err
	}
	spiffeID, err := workloadSpiffeID(a.trustDomain, a.org, rec.AgentType, rec.AgentID)
	if err != nil {
		return err
	}
	return a.entries.CreateEntry(ctx, spiffeID, a.nodeID, workloadSelectors(a.namespace, rec.AgentID, a.serviceAccount))
}

// handleKill tears down a dispatched agent: delete the org-scoped entry, then the
// local CR. Idempotent — an already-absent CR/entry is success (teardown must
// converge even after a prior partial kill).
//
// NOTE (Phase 4 vs 5): this consumer-side kill handler + the reconcile teardown
// path are complete, but the CONTROL-PLANE side that triggers them is Phase 5:
// the orchestrator's dismiss/revoke/DELETE handlers do not yet push channel.Kill
// nor mark a substrate record terminal, so a live kill is delivered only via the
// next reconcile-on-connect (a stable channel misses it). Wiring the orchestrator
// to push kills and to mark substrate DELETE/dismiss terminal is a Phase-5 task
// (see plan Phase 5 — "Kill/delete over the channel").
func (a *agent) handleKill(ctx context.Context, agentID string) {
	w, found, err := a.store.Get(ctx, agentID)
	if err != nil {
		log.Printf("substrate-agent: kill %s lookup: %v", agentID, err)
		a.report(channel.NewStatus(agentID, channel.PhaseFailed, err.Error()))
		return
	}
	if !found {
		// Nothing local — treat as already-killed.
		a.report(channel.NewStatus(agentID, channel.PhaseKilled, ""))
		return
	}
	if err := a.teardown(ctx, w); err != nil {
		log.Printf("substrate-agent: kill %s failed: %v", agentID, err)
		a.report(channel.NewStatus(agentID, channel.PhaseFailed, err.Error()))
		return
	}
	a.report(channel.NewStatus(agentID, channel.PhaseKilled, ""))
}

// teardown removes the org-scoped entry (so a torn-down workload cannot keep an
// SVID) and then the CR. The entry is deleted first: if CR delete fails we retry
// on the next reconcile, but a lingering entry (the security-relevant residue)
// is already gone.
func (a *agent) teardown(ctx context.Context, w localWorkload) error {
	spiffeID, err := workloadSpiffeID(a.trustDomain, a.org, w.AgentType, w.AgentID)
	if err != nil {
		return err
	}
	if err := a.entries.DeleteEntry(ctx, spiffeID); err != nil {
		return err
	}
	if _, err := a.store.Delete(ctx, w.AgentID); err != nil {
		return err
	}
	return nil
}

// reconcileOnce pulls the org's desired substrate records and converges local
// state to them: create should-run records with no CR, tear down local CRs whose
// record is gone/terminal. Called on every (re)connect — the durable backstop for
// spawns/kills the substrate missed while offline (no durable queue: the records
// ARE the source of truth). Best-effort per item so one failure doesn't stall the
// rest; the next reconnect retries.
func (a *agent) reconcileOnce(ctx context.Context) error {
	desired, err := a.lister.ListDesired(ctx)
	if err != nil {
		return err
	}
	actual, err := a.store.List(ctx)
	if err != nil {
		return err
	}
	toCreate, toDelete := reconcile(desired, actual)
	for _, rec := range toCreate {
		// Task is not persisted on the record, so a reconciled (offline) spawn runs
		// task-less; the live push carries the task. Consent IS persisted
		// (rec.ConsentRequired), so the CIBA gate is never dropped on reconcile.
		if err := a.materialize(ctx, rec, ""); err != nil {
			log.Printf("substrate-agent: reconcile create %s: %v", rec.AgentID, err)
			a.report(channel.NewStatus(rec.AgentID, channel.PhaseFailed, err.Error()))
			continue
		}
		a.report(channel.NewStatus(rec.AgentID, channel.PhaseCreated, ""))
	}
	for _, w := range toDelete {
		if err := a.teardown(ctx, w); err != nil {
			log.Printf("substrate-agent: reconcile teardown %s: %v", w.AgentID, err)
			continue
		}
		a.report(channel.NewStatus(w.AgentID, channel.PhaseKilled, ""))
	}
	return nil
}
