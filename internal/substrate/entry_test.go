package substrate

import (
	"context"
	"errors"
	"testing"

	"github.com/spawnly/platform/internal/substrate/devca"
)

// devcaNodePrefix is the org's x509pop node prefix (test convenience).
func devcaNodePrefix(org string) (string, error) {
	return devca.NodeSpiffePathPrefix(td, org)
}

// fakeBroker records the last spec and can be told to fail.
type fakeBroker struct {
	created []EntrySpec
	deleted []string
	failErr error
}

func (f *fakeBroker) CreateEntry(_ context.Context, spec EntrySpec) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.created = append(f.created, spec)
	return nil
}

func (f *fakeBroker) DeleteEntry(_ context.Context, spiffeID string) error {
	if f.failErr != nil {
		return f.failErr
	}
	f.deleted = append(f.deleted, spiffeID)
	return nil
}

// fakeResolver maps agentID -> orgID; unknown agentIDs are (found=false). errFor
// forces a resolver outage for a specific agentID.
type fakeResolver struct {
	orgs   map[string]string
	errFor string
}

func (f fakeResolver) RecordOrg(_ context.Context, agentID string) (string, bool, error) {
	if agentID == f.errFor {
		return "", false, errors.New("registry outage")
	}
	org, ok := f.orgs[agentID]
	return org, ok, nil
}

const td = "cluster.local"

// nodeID builds a valid x509pop node id for org (prefix + a fake fingerprint).
func nodeID(org string) string {
	p, _ := devcaNodePrefix(org)
	return p + "abc123fingerprint"
}

// good returns a fully-valid workload EntrySpec for org A owned by orgA record.
func goodSpec(org, agentID string) EntrySpec {
	return EntrySpec{
		SPIFFEID:  "spiffe://" + td + "/org/" + org + "/agent/tenant/user/type/" + agentID,
		ParentID:  nodeID(org),
		Selectors: []string{"k8s:ns:substrate-sim", "k8s:pod-name:" + agentID + "-pod", "k8s:sa:worker"},
	}
}

func newService(broker EntryBroker, resolver RecordOrgResolver) *EntryService {
	return &EntryService{Broker: broker, Resolver: resolver, TrustDomain: td}
}

// TestEntryAPIAuthzMatrix is THE test that matters for Phase 3: an org-A
// substrate-agent caller against a grid of requested entries.
func TestEntryAPIAuthzMatrix(t *testing.T) {
	resolver := fakeResolver{orgs: map[string]string{
		"agentA": "orga", // owned by org A
		"agentB": "orgb", // owned by org B
	}}

	cases := []struct {
		name      string
		callerOrg string
		spec      EntrySpec
		wantErr   bool
	}{
		{
			name:      "entry under own org A — OK",
			callerOrg: "orga",
			spec:      goodSpec("orga", "agentA"),
			wantErr:   false,
		},
		{
			name:      "entry under org B — refused",
			callerOrg: "orga",
			spec:      goodSpec("orgb", "agentB"), // SPIFFE org != caller
			wantErr:   true,
		},
		{
			name:      "parent not org-A's node — refused",
			callerOrg: "orga",
			spec: func() EntrySpec {
				s := goodSpec("orga", "agentA")
				s.ParentID = nodeID("orgb") // org B's node
				return s
			}(),
			wantErr: true,
		},
		{
			name:      "agentId owned by org B — refused",
			callerOrg: "orga",
			spec: func() EntrySpec {
				// SPIFFE org is A (matches caller) but the agentId names org B's record.
				return goodSpec("orga", "agentB")
			}(),
			wantErr: true,
		},
		{
			name:      "platform (org-less) SPIFFE ID — refused",
			callerOrg: "orga",
			spec: func() EntrySpec {
				s := goodSpec("orga", "agentA")
				s.SPIFFEID = "spiffe://" + td + "/agent/tenant/user/type/agentA"
				return s
			}(),
			wantErr: true,
		},
		{
			name:      "org-less (platform) CALLER — refused",
			callerOrg: "",
			spec:      goodSpec("orga", "agentA"),
			wantErr:   true,
		},
		{
			name:      "missing service-account selector — refused",
			callerOrg: "orga",
			spec: func() EntrySpec {
				s := goodSpec("orga", "agentA")
				s.Selectors = []string{"k8s:ns:substrate-sim", "k8s:pod-name:agentA-pod"}
				return s
			}(),
			wantErr: true,
		},
		{
			name:      "unknown agentId — refused",
			callerOrg: "orga",
			spec:      goodSpec("orga", "ghost"),
			wantErr:   true,
		},
		{
			name:      "substrate-agent identity via workload path — refused",
			callerOrg: "orga",
			spec: func() EntrySpec {
				s := goodSpec("orga", "agentA")
				s.SPIFFEID = "spiffe://" + td + "/org/orga/substrate-agent"
				return s
			}(),
			wantErr: true,
		},
		{
			name:      "wrong trust domain — refused",
			callerOrg: "orga",
			spec: func() EntrySpec {
				s := goodSpec("orga", "agentA")
				s.SPIFFEID = "spiffe://evil.example/org/orga/agent/t/u/ty/agentA"
				return s
			}(),
			wantErr: true,
		},
		{
			name:      "case-variant caller org normalizes to match — OK",
			callerOrg: "OrgA", // Keycloak slug is lowercase; must normalize
			spec:      goodSpec("orga", "agentA"),
			wantErr:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broker := &fakeBroker{}
			svc := newService(broker, resolver)
			err := svc.CreateWorkloadEntry(context.Background(), tc.callerOrg, tc.spec)
			if tc.wantErr && err == nil {
				t.Fatalf("expected refusal, got nil (broker created %d entries)", len(broker.created))
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			// A refusal must never reach the broker.
			if tc.wantErr && len(broker.created) != 0 {
				t.Fatalf("refused request still created an entry: %+v", broker.created)
			}
			if !tc.wantErr && len(broker.created) != 1 {
				t.Fatalf("authorized request did not create exactly one entry: %d", len(broker.created))
			}
		})
	}
}

func TestCreateWorkloadEntry_ResolverOutageFailsClosed(t *testing.T) {
	broker := &fakeBroker{}
	svc := newService(broker, fakeResolver{orgs: map[string]string{}, errFor: "agentA"})
	err := svc.CreateWorkloadEntry(context.Background(), "orga", goodSpec("orga", "agentA"))
	if err == nil {
		t.Fatal("expected fail-closed on resolver outage")
	}
	if errors.Is(err, ErrEntryUnauthorized) {
		t.Fatal("resolver outage must be a distinct (5xx) error, not the oracle-free unauthorized")
	}
	if len(broker.created) != 0 {
		t.Fatal("outage must not create an entry")
	}
}

func TestDeleteWorkloadEntry_OrgIsolation(t *testing.T) {
	resolver := fakeResolver{orgs: map[string]string{"agentA": "orga", "agentB": "orgb"}}
	broker := &fakeBroker{}
	svc := newService(broker, resolver)
	ctx := context.Background()

	// Own org — OK.
	if err := svc.DeleteWorkloadEntry(ctx, "orga", "spiffe://"+td+"/org/orga/agent/t/u/ty/agentA"); err != nil {
		t.Fatalf("own-org delete should succeed: %v", err)
	}
	// Cross-org SPIFFE id — refused.
	if err := svc.DeleteWorkloadEntry(ctx, "orga", "spiffe://"+td+"/org/orgb/agent/t/u/ty/agentB"); err == nil {
		t.Fatal("cross-org delete should be refused")
	}
	// Record owned by org B but id claims org A — refused (record check).
	if err := svc.DeleteWorkloadEntry(ctx, "orga", "spiffe://"+td+"/org/orga/agent/t/u/ty/agentB"); err == nil {
		t.Fatal("delete of an id whose record is org B should be refused")
	}
	// Already-gone record (found=false) but id is caller-org scoped — allowed teardown.
	if err := svc.DeleteWorkloadEntry(ctx, "orga", "spiffe://"+td+"/org/orga/agent/t/u/ty/ghost"); err != nil {
		t.Fatalf("teardown of a removed record should succeed: %v", err)
	}
}

func TestProvisionSubstrateAgentEntry(t *testing.T) {
	broker := &fakeBroker{}
	svc := newService(broker, fakeResolver{})
	ctx := context.Background()

	// Happy path: correct node + ns/sa selectors.
	sel := []string{"k8s:ns:substrate-sim", "k8s:sa:substrate-agent"}
	if err := svc.ProvisionSubstrateAgentEntry(ctx, "orga", nodeID("orga"), sel); err != nil {
		t.Fatalf("bootstrap provision should succeed: %v", err)
	}
	if len(broker.created) != 1 {
		t.Fatalf("expected one bootstrap entry, got %d", len(broker.created))
	}
	got := broker.created[0]
	want := "spiffe://" + td + "/org/orga/substrate-agent"
	if got.SPIFFEID != want {
		t.Fatalf("bootstrap SPIFFE ID = %q, want %q", got.SPIFFEID, want)
	}

	// Wrong org's node ⇒ refused.
	broker2 := &fakeBroker{}
	svc2 := newService(broker2, fakeResolver{})
	if err := svc2.ProvisionSubstrateAgentEntry(ctx, "orga", nodeID("orgb"), sel); err == nil {
		t.Fatal("bootstrap parented to another org's node must be refused")
	}
	if len(broker2.created) != 0 {
		t.Fatal("refused bootstrap must not create an entry")
	}

	// Missing SA selector ⇒ refused.
	broker3 := &fakeBroker{}
	svc3 := newService(broker3, fakeResolver{})
	if err := svc3.ProvisionSubstrateAgentEntry(ctx, "orga", nodeID("orga"), []string{"k8s:ns:substrate-sim"}); err == nil {
		t.Fatal("bootstrap without a service-account selector must be refused")
	}
}
