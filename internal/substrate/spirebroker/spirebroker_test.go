package spirebroker

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	agentv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/agent/v1"
	entryv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/entry/v1"
	"github.com/spiffe/spire-api-sdk/proto/spire/api/types"

	"github.com/spawnly/platform/internal/substrate"
	"github.com/spawnly/platform/internal/substrate/devca"
)

const testTD = "cluster.local"

// ── fake SPIRE entry admin server (in-memory, over bufconn) ────────────────────

// fakeEntryServer is a minimal, stateful stand-in for the SPIRE server entry admin
// API: it stores entries keyed by a generated id and models the behaviors the
// broker relies on — AlreadyExists on an identical create, BySpiffeId filtering,
// and BatchDelete returning NotFound for an unknown id.
type fakeEntryServer struct {
	entryv1.UnimplementedEntryServer
	mu      sync.Mutex
	entries map[string]*types.Entry // id -> entry
	nextID  int
}

func newFakeEntryServer() *fakeEntryServer {
	return &fakeEntryServer{entries: map[string]*types.Entry{}}
}

func idStr(id *types.SPIFFEID) string {
	if id == nil {
		return ""
	}
	return "spiffe://" + id.GetTrustDomain() + id.GetPath()
}

// entryKey is the identity of an entry for the AlreadyExists check: spiffe id +
// parent id (selectors ignored for the test's purposes).
func entryKey(e *types.Entry) string {
	return idStr(e.GetSpiffeId()) + "|" + idStr(e.GetParentId())
}

func (s *fakeEntryServer) BatchCreateEntry(_ context.Context, req *entryv1.BatchCreateEntryRequest) (*entryv1.BatchCreateEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &entryv1.BatchCreateEntryResponse{}
	for _, want := range req.GetEntries() {
		// Idempotency: an identical (spiffe id + parent) entry already present ⇒
		// AlreadyExists, exactly as SPIRE returns.
		exists := false
		for _, have := range s.entries {
			if entryKey(have) == entryKey(want) {
				exists = true
				break
			}
		}
		if exists {
			resp.Results = append(resp.Results, &entryv1.BatchCreateEntryResponse_Result{
				Status: &types.Status{Code: int32(codes.AlreadyExists), Message: "entry already exists"},
			})
			continue
		}
		s.nextID++
		id := "entry-" + string(rune('a'+s.nextID))
		stored := &types.Entry{Id: id, SpiffeId: want.GetSpiffeId(), ParentId: want.GetParentId(), Selectors: want.GetSelectors()}
		s.entries[id] = stored
		resp.Results = append(resp.Results, &entryv1.BatchCreateEntryResponse_Result{
			Status: &types.Status{Code: int32(codes.OK)},
			Entry:  stored,
		})
	}
	return resp, nil
}

func (s *fakeEntryServer) ListEntries(_ context.Context, req *entryv1.ListEntriesRequest) (*entryv1.ListEntriesResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var bySpiffe string
	if f := req.GetFilter(); f != nil && f.GetBySpiffeId() != nil {
		bySpiffe = idStr(f.GetBySpiffeId())
	}
	resp := &entryv1.ListEntriesResponse{}
	for _, e := range s.entries {
		if bySpiffe != "" && idStr(e.GetSpiffeId()) != bySpiffe {
			continue
		}
		resp.Entries = append(resp.Entries, e)
	}
	return resp, nil
}

func (s *fakeEntryServer) BatchDeleteEntry(_ context.Context, req *entryv1.BatchDeleteEntryRequest) (*entryv1.BatchDeleteEntryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &entryv1.BatchDeleteEntryResponse{}
	for _, id := range req.GetIds() {
		if _, ok := s.entries[id]; ok {
			delete(s.entries, id)
			resp.Results = append(resp.Results, &entryv1.BatchDeleteEntryResponse_Result{Id: id, Status: &types.Status{Code: int32(codes.OK)}})
		} else {
			resp.Results = append(resp.Results, &entryv1.BatchDeleteEntryResponse_Result{Id: id, Status: &types.Status{Code: int32(codes.NotFound), Message: "no such entry"}})
		}
	}
	return resp, nil
}

func (s *fakeEntryServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ── fake SPIRE agent admin server ──────────────────────────────────────────────

type fakeAgentServer struct {
	agentv1.UnimplementedAgentServer
	mu      sync.Mutex
	agents  map[string]bool // spiffe id -> attested
	deleted []string
}

func newFakeAgentServer(ids ...string) *fakeAgentServer {
	m := map[string]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return &fakeAgentServer{agents: m}
}

func (s *fakeAgentServer) ListAgents(_ context.Context, _ *agentv1.ListAgentsRequest) (*agentv1.ListAgentsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &agentv1.ListAgentsResponse{}
	for id := range s.agents {
		td, path, _ := strings.Cut(strings.TrimPrefix(id, "spiffe://"), "/")
		resp.Agents = append(resp.Agents, &types.Agent{Id: &types.SPIFFEID{TrustDomain: td, Path: "/" + path}})
	}
	return resp, nil
}

func (s *fakeAgentServer) DeleteAgent(_ context.Context, req *agentv1.DeleteAgentRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idStr(req.GetId())
	if !s.agents[id] {
		return nil, status.Errorf(codes.NotFound, "agent %s not attested", id)
	}
	delete(s.agents, id)
	s.deleted = append(s.deleted, id)
	return &emptypb.Empty{}, nil
}

// ── bufconn wiring ─────────────────────────────────────────────────────────────

// newTestBroker starts a bufconn gRPC server with the given fake entry/agent
// servers and returns a Broker whose REAL generated clients dial it in-memory —
// no SPIRE server, no network, no mTLS (bufconn is trusted).
func newTestBroker(t *testing.T, es entryv1.EntryServer, as agentv1.AgentServer) *Broker {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	if es != nil {
		entryv1.RegisterEntryServer(srv, es)
	}
	if as != nil {
		agentv1.RegisterAgentServer(srv, as)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return NewWithClients(entryv1.NewEntryClient(conn), agentv1.NewAgentClient(conn))
}

func nodePrefix(t *testing.T, org string) string {
	t.Helper()
	p, err := devca.NodeSpiffePathPrefix(testTD, org)
	if err != nil {
		t.Fatalf("node prefix: %v", err)
	}
	return p
}

func workloadSpec(org, agentID string) substrate.EntrySpec {
	return substrate.EntrySpec{
		SPIFFEID:  "spiffe://" + testTD + "/org/" + org + "/agent/tenant/user/type/" + agentID,
		ParentID:  "", // set by callers via nodePrefix
		Selectors: []string{"k8s:ns:substrate-sim", "k8s:pod-name:" + agentID + "-pod", "k8s:sa:worker"},
	}
}

// ── tests ──────────────────────────────────────────────────────────────────────

// TestBroker_CreateEntryIdempotent: a repeated identical create is success and
// leaves exactly one entry (AlreadyExists tolerated).
func TestBroker_CreateEntryIdempotent(t *testing.T) {
	es := newFakeEntryServer()
	b := newTestBroker(t, es, nil)
	ctx := context.Background()

	spec := workloadSpec("acme", "agent-1")
	spec.ParentID = nodePrefix(t, "acme") + "FP"

	if err := b.CreateEntry(ctx, spec); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := b.CreateEntry(ctx, spec); err != nil {
		t.Fatalf("idempotent re-create must succeed: %v", err)
	}
	if es.count() != 1 {
		t.Fatalf("want exactly 1 entry after idempotent creates, got %d", es.count())
	}
}

// TestBroker_DeleteEntry: delete removes the entry; a second (absent) delete is a
// no-op success.
func TestBroker_DeleteEntry(t *testing.T) {
	es := newFakeEntryServer()
	b := newTestBroker(t, es, nil)
	ctx := context.Background()

	spec := workloadSpec("acme", "agent-1")
	spec.ParentID = nodePrefix(t, "acme") + "FP"
	if err := b.CreateEntry(ctx, spec); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := b.DeleteEntry(ctx, spec.SPIFFEID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if es.count() != 0 {
		t.Fatalf("entry not deleted, %d remain", es.count())
	}
	// Absent delete = idempotent no-op.
	if err := b.DeleteEntry(ctx, spec.SPIFFEID); err != nil {
		t.Fatalf("delete of absent entry must be a no-op success: %v", err)
	}
}

// TestBroker_DeleteEntriesUnderParent: the sweep deletes ONLY entries parented
// under the given org-node prefix, never another org's.
func TestBroker_DeleteEntriesUnderParent(t *testing.T) {
	es := newFakeEntryServer()
	b := newTestBroker(t, es, nil)
	ctx := context.Background()

	acmePrefix := nodePrefix(t, "acme")
	globexPrefix := nodePrefix(t, "globex")

	// Two acme entries under the OLD acme node, plus a globex entry.
	acmeBootstrap := substrate.EntrySpec{SPIFFEID: "spiffe://" + testTD + "/org/acme/substrate-agent", ParentID: acmePrefix + "OLDFP", Selectors: []string{"k8s:ns:substrate-sim", "k8s:sa:substrate-agent"}}
	acmeWorkload := workloadSpec("acme", "agent-1")
	acmeWorkload.ParentID = acmePrefix + "OLDFP"
	globexWorkload := workloadSpec("globex", "agent-9")
	globexWorkload.ParentID = globexPrefix + "GFP"
	for _, s := range []substrate.EntrySpec{acmeBootstrap, acmeWorkload, globexWorkload} {
		if err := b.CreateEntry(ctx, s); err != nil {
			t.Fatalf("seed create: %v", err)
		}
	}

	if err := b.DeleteEntriesUnderParent(ctx, acmePrefix); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	// Only globex's entry should remain.
	if es.count() != 1 {
		t.Fatalf("want 1 entry remaining (globex), got %d", es.count())
	}
	for _, e := range es.entries {
		if !strings.Contains(idStr(e.GetSpiffeId()), "/org/globex/") {
			t.Fatalf("sweep removed the wrong entry / left an acme entry: %s", idStr(e.GetSpiffeId()))
		}
	}
}

// An empty parent prefix must never be treated as "everything".
func TestBroker_DeleteEntriesUnderParent_RefusesEmpty(t *testing.T) {
	b := newTestBroker(t, newFakeEntryServer(), nil)
	if err := b.DeleteEntriesUnderParent(context.Background(), ""); err == nil {
		t.Fatal("empty prefix must error, not sweep every entry")
	}
}

// TestBroker_EvictNodesUnderPrefix: evict the org's superseded nodes, keep the new
// node, and never touch another org's or the platform's nodes; count is accurate.
func TestBroker_EvictNodesUnderPrefix(t *testing.T) {
	acmePrefix := nodePrefix(t, "acme")
	globexPrefix := nodePrefix(t, "globex")
	keep := acmePrefix + "NEWFP"
	as := newFakeAgentServer(
		acmePrefix+"OLDFP1",
		acmePrefix+"OLDFP2",
		keep,
		globexPrefix+"GFP",
		"spiffe://"+testTD+"/spire/agent/k8s_psat/agent-platform/NODEUID",
	)
	b := newTestBroker(t, nil, as)

	n, err := b.EvictNodesUnderPrefix(context.Background(), acmePrefix, keep)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if n != 2 {
		t.Fatalf("evicted count = %d, want 2 (the count the connect audits)", n)
	}
	got := map[string]bool{}
	for _, id := range as.deleted {
		got[id] = true
	}
	if !got[acmePrefix+"OLDFP1"] || !got[acmePrefix+"OLDFP2"] {
		t.Fatalf("did not evict acme's superseded nodes: %v", as.deleted)
	}
	if got[keep] {
		t.Fatal("evicted the node the NEW leaf attests as")
	}
	for _, forbidden := range []string{globexPrefix + "GFP", "spiffe://" + testTD + "/spire/agent/k8s_psat/agent-platform/NODEUID"} {
		if got[forbidden] {
			t.Fatalf("evicted a node outside the org prefix (%s) — isolation breach", forbidden)
		}
	}
}

func TestBroker_EvictNodesUnderPrefix_RefusesEmpty(t *testing.T) {
	b := newTestBroker(t, nil, newFakeAgentServer())
	if _, err := b.EvictNodesUnderPrefix(context.Background(), "", ""); err == nil {
		t.Fatal("empty prefix must error, not evict every node")
	}
}

// ── broker-error / fail-closed ─────────────────────────────────────────────────

// erroringEntryAPI is an EntryAPI whose every call fails — modeling a broker whose
// mTLS source / connection is unusable (Unavailable). It must surface as a broker
// error, NOT the oracle-free authz error, so the handler renders a 502.
type erroringEntryAPI struct{}

func (erroringEntryAPI) BatchCreateEntry(context.Context, *entryv1.BatchCreateEntryRequest, ...grpc.CallOption) (*entryv1.BatchCreateEntryResponse, error) {
	return nil, status.Error(codes.Unavailable, "no mTLS identity")
}
func (erroringEntryAPI) ListEntries(context.Context, *entryv1.ListEntriesRequest, ...grpc.CallOption) (*entryv1.ListEntriesResponse, error) {
	return nil, status.Error(codes.Unavailable, "no mTLS identity")
}
func (erroringEntryAPI) BatchDeleteEntry(context.Context, *entryv1.BatchDeleteEntryRequest, ...grpc.CallOption) (*entryv1.BatchDeleteEntryResponse, error) {
	return nil, status.Error(codes.Unavailable, "no mTLS identity")
}

// TestEntryService_BrokerFailureFailsClosed: an AUTHORIZED create whose broker call
// fails returns a non-authz error (⇒ 502 fail-closed), never a silent success and
// never the oracle-free ErrEntryUnauthorized.
func TestEntryService_BrokerFailureFailsClosed(t *testing.T) {
	broker := NewWithClients(erroringEntryAPI{}, nil)
	svc := &substrate.EntryService{
		Broker:      broker,
		Resolver:    fakeResolver{"agent-1": "acme"},
		TrustDomain: testTD,
	}
	spec := workloadSpec("acme", "agent-1")
	spec.ParentID = nodePrefix(t, "acme") + "FP"

	err := svc.CreateWorkloadEntry(context.Background(), "acme", spec)
	if err == nil {
		t.Fatal("broker failure must not be a silent success")
	}
	if errors.Is(err, substrate.ErrEntryUnauthorized) {
		t.Fatal("broker outage must be a distinct (5xx) error, not the oracle-free unauthorized")
	}
}

// ── authz-unmoved: the EntryService authz matrix re-run against the gRPC broker ──

type fakeResolver map[string]string // agentID -> orgID

func (f fakeResolver) RecordOrg(_ context.Context, agentID string) (string, bool, error) {
	org, ok := f[agentID]
	return org, ok, nil
}

// TestEntryServiceAuthz_ViaGRPCBroker proves the authorization is UNMOVED by the
// broker swap: an org-A caller's grid of requests is authorized identically, and a
// REFUSED request never reaches the (real, bufconn) gRPC broker — no entry is ever
// created for a request the EntryService rejects.
func TestEntryServiceAuthz_ViaGRPCBroker(t *testing.T) {
	acmePrefix := nodePrefix(t, "acme")
	globexPrefix := nodePrefix(t, "globex")

	cases := []struct {
		name      string
		callerOrg string
		mutate    func(s *substrate.EntrySpec)
		wantErr   bool
	}{
		{name: "own org A — OK", callerOrg: "acme", mutate: func(s *substrate.EntrySpec) { s.ParentID = acmePrefix + "FP" }, wantErr: false},
		{name: "cross-org SPIFFE id — refused", callerOrg: "acme", mutate: func(s *substrate.EntrySpec) {
			s.SPIFFEID = "spiffe://" + testTD + "/org/globex/agent/tenant/user/type/agent-2"
			s.ParentID = globexPrefix + "FP"
		}, wantErr: true},
		{name: "parent not caller's node — refused", callerOrg: "acme", mutate: func(s *substrate.EntrySpec) { s.ParentID = globexPrefix + "FP" }, wantErr: true},
		{name: "missing SA selector — refused", callerOrg: "acme", mutate: func(s *substrate.EntrySpec) {
			s.ParentID = acmePrefix + "FP"
			s.Selectors = []string{"k8s:ns:substrate-sim", "k8s:pod-name:x"}
		}, wantErr: true},
		{name: "org-less caller — refused", callerOrg: "", mutate: func(s *substrate.EntrySpec) { s.ParentID = acmePrefix + "FP" }, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			es := newFakeEntryServer()
			broker := newTestBroker(t, es, nil)
			svc := &substrate.EntryService{
				Broker:      broker,
				Resolver:    fakeResolver{"agent-1": "acme", "agent-2": "globex"},
				TrustDomain: testTD,
			}
			spec := workloadSpec("acme", "agent-1")
			tc.mutate(&spec)
			err := svc.CreateWorkloadEntry(context.Background(), tc.callerOrg, spec)
			if tc.wantErr && err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			// A refusal must NEVER reach the gRPC broker.
			if tc.wantErr && es.count() != 0 {
				t.Fatalf("refused request still created a broker entry (%d) — authz moved into the broker", es.count())
			}
			if !tc.wantErr && es.count() != 1 {
				t.Fatalf("authorized request did not create exactly one broker entry: %d", es.count())
			}
		})
	}
}

// TestNew_WorkloadAPIUnavailableFailsClosed: New against an unreachable Workload
// API within a bounded context returns an error (nil broker) — construction fails
// closed rather than hanging or yielding an unauthenticated broker.
func TestNew_WorkloadAPIUnavailableFailsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := New(ctx, Config{
		Addr:              "127.0.0.1:0",
		WorkloadAPISocket: "unix:///nonexistent/spire-agent-does-not-exist.sock",
		TrustDomain:       testTD,
	})
	if err == nil {
		t.Fatalf("expected fail-closed error, got broker %v", b)
	}
}
