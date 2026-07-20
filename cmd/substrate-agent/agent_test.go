package main

import (
	"context"
	"errors"
	"sort"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate/channel"
)

// ── fakes ───────────────────────────────────────────────────────────────────

type fakeStore struct {
	created map[string]registry.AgentRecord
	tasks   map[string]string
	deleted []string
}

func newFakeStore(seed ...localWorkload) *fakeStore {
	s := &fakeStore{created: map[string]registry.AgentRecord{}, tasks: map[string]string{}}
	for _, w := range seed {
		s.created[w.AgentID] = registry.AgentRecord{AgentID: w.AgentID, AgentType: w.AgentType}
	}
	return s
}
func (s *fakeStore) Create(_ context.Context, rec registry.AgentRecord, task string) error {
	s.created[rec.AgentID] = rec
	s.tasks[rec.AgentID] = task
	return nil
}
func (s *fakeStore) Get(_ context.Context, id string) (localWorkload, bool, error) {
	rec, ok := s.created[id]
	if !ok {
		return localWorkload{}, false, nil
	}
	return localWorkload{AgentID: rec.AgentID, AgentType: rec.AgentType}, true, nil
}
func (s *fakeStore) Delete(_ context.Context, id string) (bool, error) {
	_, ok := s.created[id]
	delete(s.created, id)
	s.deleted = append(s.deleted, id)
	return ok, nil
}
func (s *fakeStore) List(_ context.Context) ([]localWorkload, error) {
	out := make([]localWorkload, 0, len(s.created))
	for _, rec := range s.created {
		out = append(out, localWorkload{AgentID: rec.AgentID, AgentType: rec.AgentType})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out, nil
}

type fakeEntries struct {
	created map[string]string // spiffeID -> parentID
	deleted []string
	err     error
}

func newFakeEntries() *fakeEntries { return &fakeEntries{created: map[string]string{}} }
func (e *fakeEntries) CreateEntry(_ context.Context, spiffeID, parentID string, _ []string) error {
	if e.err != nil {
		return e.err
	}
	e.created[spiffeID] = parentID
	return nil
}
func (e *fakeEntries) DeleteEntry(_ context.Context, spiffeID string) error {
	e.deleted = append(e.deleted, spiffeID)
	return nil
}

type fakeLister struct{ recs []registry.AgentRecord }

func (l fakeLister) ListDesired(context.Context) ([]registry.AgentRecord, error) { return l.recs, nil }

func newTestAgent(store workloadStore, entries entryClient, lister desiredLister) *agent {
	return &agent{
		trustDomain:    "cluster.local",
		org:            "acme",
		namespace:      "substrate-sim",
		nodeID:         "spiffe://cluster.local/spire/agent/x509pop/org/acme/deadbeef",
		serviceAccount: "default",
		store:          store,
		entries:        entries,
		lister:         lister,
	}
}

// ── handleSpawn / handleKill ────────────────────────────────────────────────

func TestHandleSpawn_CreatesCRAndOrgScopedEntry(t *testing.T) {
	store, entries := newFakeStore(), newFakeEntries()
	a := newTestAgent(store, entries, fakeLister{})
	a.handleSpawn(context.Background(), channel.SpawnCommand{
		AgentID: "agent-1", AgentType: "worker", UserID: "u", Task: "do",
		// A lying org in the frame MUST be ignored: the entry uses a.org.
		OrgID: "globex",
	})
	if _, ok := store.created["agent-1"]; !ok {
		t.Fatal("CR not created")
	}
	if store.created["agent-1"].OrgID != "acme" {
		t.Fatalf("CR org = %q, want acme (from SVID, never the frame)", store.created["agent-1"].OrgID)
	}
	want := "spiffe://cluster.local/org/acme/agent/worker/agent-1"
	if _, ok := entries.created[want]; !ok {
		t.Fatalf("entry %q not created; have %v", want, entries.created)
	}
	if entries.created[want] != a.nodeID {
		t.Fatalf("entry parent = %q, want %q", entries.created[want], a.nodeID)
	}
}

func TestHandleSpawn_EntryFailureReported(t *testing.T) {
	store, entries := newFakeStore(), newFakeEntries()
	entries.err = errors.New("entry API 403")
	var reports []channel.Up
	a := newTestAgent(store, entries, fakeLister{})
	a.send = func(u channel.Up) { reports = append(reports, u) }
	a.handleSpawn(context.Background(), channel.SpawnCommand{AgentID: "agent-1", AgentType: "worker", UserID: "u"})
	var sawFailed bool
	for _, r := range reports {
		if r.Type == channel.TypeStatus && r.Status.Phase == channel.PhaseFailed {
			sawFailed = true
		}
	}
	if !sawFailed {
		t.Fatalf("expected a failed status report, got %+v", reports)
	}
}

func TestHandleKill_DeletesEntryThenCR(t *testing.T) {
	store := newFakeStore(localWorkload{AgentID: "agent-1", AgentType: "worker"})
	entries := newFakeEntries()
	a := newTestAgent(store, entries, fakeLister{})
	a.handleKill(context.Background(), "agent-1")
	if len(entries.deleted) != 1 || entries.deleted[0] != "spiffe://cluster.local/org/acme/agent/worker/agent-1" {
		t.Fatalf("entry not deleted: %v", entries.deleted)
	}
	if _, ok := store.created["agent-1"]; ok {
		t.Fatal("CR not deleted")
	}
}

// ── reconcile pure ──────────────────────────────────────────────────────────

func TestReconcile_CreateAndDeleteDelta(t *testing.T) {
	desired := []registry.AgentRecord{
		{AgentID: "keep", AgentType: "worker", Status: "active", Placement: registry.PlacementSubstrate},
		{AgentID: "new", AgentType: "worker", Status: "pending", Placement: registry.PlacementSubstrate},
		{AgentID: "done", AgentType: "worker", Status: "completed", Placement: registry.PlacementSubstrate},
	}
	actual := []localWorkload{
		{AgentID: "keep", AgentType: "worker"},
		{AgentID: "done", AgentType: "worker"}, // terminal record ⇒ tear down
		{AgentID: "gone", AgentType: "worker"}, // no record ⇒ tear down
	}
	toCreate, toDelete := reconcile(desired, actual)
	if len(toCreate) != 1 || toCreate[0].AgentID != "new" {
		t.Fatalf("toCreate = %+v, want [new]", toCreate)
	}
	delIDs := map[string]bool{}
	for _, w := range toDelete {
		delIDs[w.AgentID] = true
	}
	if !delIDs["done"] || !delIDs["gone"] || delIDs["keep"] {
		t.Fatalf("toDelete = %+v, want {done,gone}", toDelete)
	}
}

func TestShouldRun(t *testing.T) {
	cases := map[string]bool{"pending": true, "active": true, "revoked": true, "completed": false, "failed": false}
	for status, want := range cases {
		if got := shouldRun(registry.AgentRecord{Status: status}); got != want {
			t.Errorf("shouldRun(%s) = %v, want %v", status, got, want)
		}
	}
	if shouldRun(registry.AgentRecord{Status: "active", Dismissed: true}) {
		t.Error("a dismissed record must not run")
	}
}

// TestReconcileOnce_OfflineSpawnAndKillAppliedOnReconnect: the offline spawn
// (a pending record with no local CR) is created, and the offline kill (a local
// CR whose record is gone) is torn down — desired-vs-actual convergence, the
// no-durable-queue backstop.
func TestReconcileOnce_OfflineSpawnAndKillAppliedOnReconnect(t *testing.T) {
	// Local CR "orphan" exists but has no desired record ⇒ must be torn down.
	store := newFakeStore(localWorkload{AgentID: "orphan", AgentType: "worker"})
	entries := newFakeEntries()
	lister := fakeLister{recs: []registry.AgentRecord{
		{AgentID: "offline-spawn", AgentType: "worker", Status: "pending", OrgID: "acme", Placement: registry.PlacementSubstrate},
	}}
	a := newTestAgent(store, entries, lister)

	if err := a.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, ok := store.created["offline-spawn"]; !ok {
		t.Fatal("offline spawn not materialized on reconnect")
	}
	if _, ok := entries.created["spiffe://cluster.local/org/acme/agent/worker/offline-spawn"]; !ok {
		t.Fatalf("offline spawn entry not created: %v", entries.created)
	}
	if _, ok := store.created["orphan"]; ok {
		t.Fatal("orphan CR not torn down on reconnect")
	}
	if len(entries.deleted) != 1 || entries.deleted[0] != "spiffe://cluster.local/org/acme/agent/worker/orphan" {
		t.Fatalf("orphan entry not deleted: %v", entries.deleted)
	}
}

// The reconcile path must preserve a persisted consent gate (no consent bypass).
func TestReconcileOnce_PreservesConsentGate(t *testing.T) {
	store, entries := newFakeStore(), newFakeEntries()
	lister := fakeLister{recs: []registry.AgentRecord{
		{AgentID: "child", AgentType: "worker", Status: "pending", OrgID: "acme", Placement: registry.PlacementSubstrate, ConsentRequired: true},
	}}
	a := newTestAgent(store, entries, lister)
	if err := a.reconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !store.created["child"].ConsentRequired {
		t.Fatal("reconcile dropped ConsentRequired — consent bypass")
	}
}
