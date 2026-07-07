package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

// seedAuthzWorld builds a store + SpiceDB mock modelling the spec's authorization
// matrix world:
//
//	agent "agent-acme"  — org=acme,  owner=alice
//	agent "agent-less"  — org-less,  owner=frank
//	org "acme": alice=member(+owner), carol=admin, dave=member
//	org "globex": eve=member  (cross-org)
//	user "gina@corp.io" — admin of acme, addressed by an EXOTIC userId that must
//	   survive spicedb.EncodeObjectID identically on the write and read sides.
//
// Tuples are seeded exactly as production writes them (registration owner/org
// tuples in main.go, membership tuples via orgTuple) — including the same
// EncodeObjectID on every subject — so the check side (agentAllowed) agrees.
func seedAuthzWorld(t *testing.T) (registry.Store, spicedb.Client) {
	t.Helper()
	ctx := context.Background()
	s := newStore()
	sdb := spicedb.NewMock()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(s.RegisterAgent(ctx, registry.AgentRecord{AgentID: "agent-acme", AgentType: "worker", UserID: "alice", OrgID: "acme", Status: "active"}))
	must(s.RegisterAgent(ctx, registry.AgentRecord{AgentID: "agent-less", AgentType: "worker", UserID: "frank", Status: "active"}))

	enc := spicedb.EncodeObjectID
	// Agent lifecycle tuples (owner + org) — as registration writes them.
	must(sdb.WriteRelationship(ctx, "agent:agent-acme", "owner", "user:"+enc("alice")))
	must(sdb.WriteRelationship(ctx, "agent:agent-acme", "org", "organization:"+enc("acme")))
	must(sdb.WriteRelationship(ctx, "agent:agent-less", "owner", "user:"+enc("frank")))
	// Membership tuples — as orgTuple writes them.
	must(sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "member", "user:"+enc("alice")))
	must(sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "admin", "user:"+enc("carol")))
	must(sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "member", "user:"+enc("dave")))
	must(sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "admin", "user:"+enc("gina@corp.io")))
	must(sdb.WriteRelationship(ctx, "organization:"+enc("globex"), "member", "user:"+enc("eve")))
	return s, sdb
}

// TestAgentAllowed_Matrix is the Phase-4 authorization matrix at the registry
// tier, replacing the old userId-equality ownership test. Rows cover
// {owner-member, other-member, org-admin, cross-org, exotic-id admin, org-less
// owner, org-less non-owner, empty} × {view, manage}.
func TestAgentAllowed_Matrix(t *testing.T) {
	s, sdb := seedAuthzWorld(t)
	ctx := context.Background()

	cases := []struct {
		name              string
		agent, user       string
		wantView, wantMng bool
		wantExists        bool
	}{
		{"owner is member: full control", "agent-acme", "alice", true, true, true},
		{"org admin (not owner): full control", "agent-acme", "carol", true, true, true},
		{"other member: read-only", "agent-acme", "dave", true, false, true},
		{"cross-org member: invisible", "agent-acme", "eve", false, false, true},
		{"exotic-id org admin: full control", "agent-acme", "gina@corp.io", true, true, true},
		{"empty user: denied", "agent-acme", "", false, false, true},
		{"org-less owner: full control (equality)", "agent-less", "frank", true, true, true},
		{"org-less non-owner: denied (equality, org ignored)", "agent-less", "dave", false, false, true},
		{"unknown agent: not found", "agent-nope", "alice", false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotView, existsV, err := agentAllowed(ctx, s, sdb, c.agent, c.user, actionView)
			if err != nil {
				t.Fatalf("view: unexpected err: %v", err)
			}
			gotMng, existsM, err := agentAllowed(ctx, s, sdb, c.agent, c.user, actionManage)
			if err != nil {
				t.Fatalf("manage: unexpected err: %v", err)
			}
			if existsV != c.wantExists || existsM != c.wantExists {
				t.Errorf("exists = %v/%v, want %v", existsV, existsM, c.wantExists)
			}
			if gotView != c.wantView {
				t.Errorf("view = %v, want %v", gotView, c.wantView)
			}
			if gotMng != c.wantMng {
				t.Errorf("manage = %v, want %v", gotMng, c.wantMng)
			}
		})
	}
}

// spicedbErr is a spicedb.Client whose CheckPermission always errors, to prove
// the org-scoped check FAILS CLOSED (deny, not a 5xx oracle and not open).
type spicedbErr struct{ spicedb.Client }

func (spicedbErr) CheckPermission(context.Context, string, string, string) (bool, error) {
	return false, context.DeadlineExceeded
}

func TestAgentAllowed_SpiceDBErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	must := func(err error) {
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(s.RegisterAgent(ctx, registry.AgentRecord{AgentID: "agent-acme", AgentType: "worker", UserID: "alice", OrgID: "acme", Status: "active"}))

	sdb := spicedbErr{Client: spicedb.NewMock()}
	// The agent EXISTS but SpiceDB errors → deny (allowed=false) with exists=true
	// and NO error propagated (so the handler returns 404, never a 5xx that leaks
	// the id or an open grant).
	allowed, exists, err := agentAllowed(ctx, s, sdb, "agent-acme", "alice", actionManage)
	if err != nil {
		t.Fatalf("spicedb error must NOT propagate as a store error, got %v", err)
	}
	if allowed {
		t.Error("spicedb error must fail CLOSED (allowed=false)")
	}
	if !exists {
		t.Error("exists should stay true — the record was read fine")
	}
}

// TestOrgAgentHandlers_ManageWiring proves the lifecycle mutation handlers
// (revoke here, representing dismiss/resume/subtree) are wired to MANAGE authority
// for org-scoped agents: a same-org read-only member is denied 404 while the org
// admin and the owner succeed. This is the HTTP-level counterpart to the
// agentAllowed matrix, confirming the handlers pass sdb + the token userId through.
func TestOrgAgentHandlers_ManageWiring(t *testing.T) {
	s, sdb := seedAuthzWorld(t)
	s.(*store).putTemplate(workerTemplate())
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll(), testDir())

	revoke := func(user string) int {
		req := httptest.NewRequest("POST", "/v1/agents/agent-acme/revoke?userId="+user, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	subtree := func(user string) int {
		req := httptest.NewRequest("GET", "/v1/agents/agent-acme/subtree?userId="+user, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}

	// Read-only member (dave) → 404 on a manage op AND on the manage-gated subtree.
	if got := subtree("dave"); got != http.StatusNotFound {
		t.Errorf("member subtree: got %d, want 404 (manage-gated)", got)
	}
	if got := revoke("dave"); got != http.StatusNotFound {
		t.Errorf("member revoke: got %d, want 404", got)
	}
	// Cross-org user (eve) → 404.
	if got := revoke("eve"); got != http.StatusNotFound {
		t.Errorf("cross-org revoke: got %d, want 404", got)
	}
	// Org admin (carol, not owner) → 200 (manage).
	if got := subtree("carol"); got != http.StatusOK {
		t.Errorf("admin subtree: got %d, want 200", got)
	}
	if got := revoke("carol"); got != http.StatusOK {
		t.Errorf("admin revoke: got %d, want 200", got)
	}
}

// TestAgentAllowed_OrgLessSkipsSpiceDB proves an org-less agent never triggers a
// SpiceDB round-trip (equality only): a CheckPermission-erroring client is fine
// because it is never called, so an org-less owner is still admitted.
func TestAgentAllowed_OrgLessSkipsSpiceDB(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	if err := s.RegisterAgent(ctx, registry.AgentRecord{AgentID: "agent-less", AgentType: "worker", UserID: "frank", Status: "active"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	sdb := spicedbErr{Client: spicedb.NewMock()} // would error IF called
	allowed, _, err := agentAllowed(ctx, s, sdb, "agent-less", "frank", actionManage)
	if err != nil || !allowed {
		t.Fatalf("org-less owner must be admitted via equality without any SpiceDB call, got allowed=%v err=%v", allowed, err)
	}
}
