// internal/operator/stalesweep_test.go
package operator_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/operator"
	"github.com/spawnly/platform/internal/registry"
)

// workload is an AgentWorkload CR whose name (== agentId) is what the sweep
// matches records against.
func workload(name string) *agentv1alpha1.AgentWorkload {
	return workloadIn(name, "default")
}

func workloadIn(name, namespace string) *agentv1alpha1.AgentWorkload {
	return &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
}

func agent(id, status string) registry.AgentRecord {
	return registry.AgentRecord{AgentID: id, AgentType: "worker", Status: status}
}

// errListAgents wraps registry.Mock to force ListAgents to error, exercising the
// fail-safe path (the base Mock never errors).
type errListAgents struct {
	*registry.Mock
	err error
}

func (e errListAgents) ListAgents(context.Context) ([]registry.AgentRecord, error) {
	return nil, e.err
}

// errFailOn wraps registry.Mock to make Fail error for one agent id, exercising
// the per-agent skip-and-continue path (the base Mock's Fail never errors).
type errFailOn struct {
	*registry.Mock
	failID string
}

func (e errFailOn) Fail(ctx context.Context, id string) error {
	if id == e.failID {
		return errors.New("patch refused")
	}
	return e.Mock.Fail(ctx, id)
}

// errReader stubs the controller-runtime reader with a List that always errors,
// exercising the catastrophic half of the fail-safe (an errored workload read
// must never fail every agent).
type errReader struct{ err error }

func (r errReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func (r errReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return r.err
}

func newSweeper(t *testing.T, reg registry.Client, ev events.Client, objs ...client.Object) *operator.StaleSweeper {
	t.Helper()
	s := buildScheme(t)
	fc := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &operator.StaleSweeper{
		Registry:  reg,
		Events:    ev,
		Reader:    fc,
		Namespace: "default",
	}
}

func TestSweep_OrphansMarkedFailedWithEvent(t *testing.T) {
	reg := registry.NewMock(nil)
	// Three non-terminal records in the workload-backed statuses, all orphaned
	// (no matching AgentWorkload exists).
	reg.Agents = []registry.AgentRecord{
		agent("a-active", "active"),
		agent("a-consent", "awaiting-consent"),
		agent("a-revoked", "revoked"),
	}
	ev := events.NewMock()

	// No workloads seeded — every record is an orphan.
	sweeper := newSweeper(t, reg, ev)
	if err := sweeper.Sweep(context.Background(), "startup"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	wantFailed := map[string]bool{"a-active": true, "a-consent": true, "a-revoked": true}
	if len(reg.Failed) != len(wantFailed) {
		t.Fatalf("Failed = %v, want the 3 orphans", reg.Failed)
	}
	for _, id := range reg.Failed {
		if !wantFailed[id] {
			t.Errorf("unexpected agent failed: %q", id)
		}
	}
	for id := range wantFailed {
		evs := ev.Events[id]
		if len(evs) != 1 {
			t.Errorf("agent %q: got %d events, want 1", id, len(evs))
			continue
		}
		if evs[0].Source != events.SourceOperator || evs[0].Type != "reconcile.stale" {
			t.Errorf("agent %q: event = %+v, want OPERATOR/reconcile.stale", id, evs[0])
		}
		// The trigger is threaded into the audit payload so a resync-driven kill
		// isn't misattributed to boot.
		if !strings.Contains(string(evs[0].Payload), "startup reconciliation") {
			t.Errorf("agent %q: payload %s does not name the startup trigger", id, evs[0].Payload)
		}
	}
}

// TestSweep_RemoteAgentsExcluded pins the Phase-10 invariant: a consumer-hosted
// (Placement=remote) agent has NO AgentWorkload by design, so the sweep must
// NEVER fail it — otherwise every remote agent dies on the operator's first
// startup sweep. A co-located platform (k8s) orphan in the same active state IS
// still swept, proving the skip is scoped to remote placement, not to status.
func TestSweep_RemoteAgentsExcluded(t *testing.T) {
	reg := registry.NewMock(nil)
	remote := agent("a-remote", "active")
	remote.Placement = registry.PlacementRemote
	reg.Agents = []registry.AgentRecord{
		remote,               // remote, no workload — must be left alone
		agent("a-k8s", "active"), // platform orphan, no workload — must be swept
	}
	ev := events.NewMock()

	// No workloads seeded — the k8s record is an orphan, the remote one is not.
	sweeper := newSweeper(t, reg, ev)
	if err := sweeper.Sweep(context.Background(), "startup"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(reg.Failed) != 1 || reg.Failed[0] != "a-k8s" {
		t.Fatalf("Failed = %v, want [a-k8s] only (remote agent must be excluded)", reg.Failed)
	}
	if len(ev.Events["a-remote"]) != 0 {
		t.Errorf("remote agent got sweep events %v, want none", ev.Events["a-remote"])
	}
}

func TestSweep_MatchingWorkloadUntouched(t *testing.T) {
	reg := registry.NewMock(nil)
	reg.Agents = []registry.AgentRecord{
		agent("live", "active"), // has a workload → keep
		agent("dead", "active"), // no workload → sweep
	}
	ev := events.NewMock()

	sweeper := newSweeper(t, reg, ev, workload("live"))
	if err := sweeper.Sweep(context.Background(), "startup"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(reg.Failed) != 1 || reg.Failed[0] != "dead" {
		t.Fatalf("Failed = %v, want [dead] only (live has a workload)", reg.Failed)
	}
	if len(ev.Events["live"]) != 0 {
		t.Errorf("live agent got events, want none: %+v", ev.Events["live"])
	}
}

func TestSweep_WorkloadInOtherNamespaceReadsAsOrphan(t *testing.T) {
	reg := registry.NewMock(nil)
	reg.Agents = []registry.AgentRecord{agent("elsewhere", "active")}
	ev := events.NewMock()

	// The workload exists, but in a different namespace from the one the sweeper
	// watches — the List is namespace-scoped, so the record reads as orphaned.
	// This pins the scoping: AGENT_NAMESPACE must match where workloads live.
	sweeper := newSweeper(t, reg, ev, workloadIn("elsewhere", "other-ns"))
	if err := sweeper.Sweep(context.Background(), "startup"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(reg.Failed) != 1 || reg.Failed[0] != "elsewhere" {
		t.Fatalf("Failed = %v, want [elsewhere] (workload in another namespace is invisible)", reg.Failed)
	}
}

func TestSweep_NonSweptStatusesUntouched(t *testing.T) {
	reg := registry.NewMock(nil)
	// pending (in-flight spawn preregistered before its workload), completed and
	// failed (terminal) must never be swept, even with no workload present.
	reg.Agents = []registry.AgentRecord{
		agent("p", "pending"),
		agent("c", "completed"),
		agent("f", "failed"),
	}
	ev := events.NewMock()

	sweeper := newSweeper(t, reg, ev)
	if err := sweeper.Sweep(context.Background(), "startup"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(reg.Failed) != 0 {
		t.Fatalf("Failed = %v, want none (pending/completed/failed excluded)", reg.Failed)
	}
	if len(ev.Events) != 0 {
		t.Errorf("posted events, want none: %+v", ev.Events)
	}
}

func TestSweep_ListAgentsErrorPatchesNothing(t *testing.T) {
	base := registry.NewMock(nil)
	base.Agents = []registry.AgentRecord{agent("a-active", "active")}
	reg := errListAgents{Mock: base, err: errors.New("registry down")}
	ev := events.NewMock()

	// An orphan exists, but the registry read fails → fail-safe aborts.
	sweeper := newSweeper(t, reg, ev)
	if err := sweeper.Sweep(context.Background(), "startup"); err == nil {
		t.Fatal("Sweep returned nil, want the ListAgents error")
	}

	if len(base.Failed) != 0 {
		t.Fatalf("Failed = %v, want none on a read error", base.Failed)
	}
	if len(ev.Events) != 0 {
		t.Errorf("posted events on a read error, want none: %+v", ev.Events)
	}
}

func TestSweep_WorkloadListErrorPatchesNothing(t *testing.T) {
	reg := registry.NewMock(nil)
	reg.Agents = []registry.AgentRecord{agent("a-active", "active")}
	ev := events.NewMock()

	// The catastrophic half of the fail-safe: an errored workload read must
	// abort, NOT read as "zero workloads" and fail every live agent.
	sweeper := &operator.StaleSweeper{
		Registry:  reg,
		Events:    ev,
		Reader:    errReader{err: errors.New("cache not ready")},
		Namespace: "default",
	}
	if err := sweeper.Sweep(context.Background(), "startup"); err == nil {
		t.Fatal("Sweep returned nil, want the workload List error")
	}

	if len(reg.Failed) != 0 {
		t.Fatalf("Failed = %v, want none on a workload read error", reg.Failed)
	}
	if len(ev.Events) != 0 {
		t.Errorf("posted events on a workload read error, want none: %+v", ev.Events)
	}
}

func TestSweep_PerAgentFailErrorSkipsAndContinues(t *testing.T) {
	base := registry.NewMock(nil)
	base.Agents = []registry.AgentRecord{
		agent("stuck", "active"), // Fail errors → skipped, no event
		agent("ok", "active"),    // Fail succeeds → swept + event
	}
	reg := errFailOn{Mock: base, failID: "stuck"}
	ev := events.NewMock()

	sweeper := newSweeper(t, reg, ev)
	if err := sweeper.Sweep(context.Background(), "startup"); err != nil {
		t.Fatalf("Sweep: %v (per-agent Fail errors must not abort the sweep)", err)
	}

	if len(base.Failed) != 1 || base.Failed[0] != "ok" {
		t.Fatalf("Failed = %v, want [ok] only (stuck errored and was skipped)", base.Failed)
	}
	if len(ev.Events["stuck"]) != 0 {
		t.Errorf("stuck agent got events despite a failed PATCH: %+v", ev.Events["stuck"])
	}
	if len(ev.Events["ok"]) != 1 {
		t.Errorf("ok agent: got %d events, want 1", len(ev.Events["ok"]))
	}
}
