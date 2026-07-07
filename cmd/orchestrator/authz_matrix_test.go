package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fakeclient "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

// orgAuthzRegistry serves records for the orchestrator-tier matrix: an org agent
// (acme, owner alice) and an org-less agent (owner frank).
func orgAuthzRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	rec := func(id, user, org string) registry.AgentRecord {
		return registry.AgentRecord{AgentID: id, AgentType: "worker", UserID: user, OrgID: org}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/agents/agent-acme":
			json.NewEncoder(w).Encode(rec("agent-acme", "alice", "acme"))
		case "/v1/agents/agent-less":
			json.NewEncoder(w).Encode(rec("agent-less", "frank", ""))
		default:
			http.NotFound(w, r)
		}
	}))
}

// seedOrgTuples writes the membership + agent-org tuples into a mock exactly as
// production would (encoded subjects), so the orchestrator's SpiceDB checks pass.
func seedOrgTuples(sdb *spicedb.Mock) {
	ctx := context.Background()
	enc := spicedb.EncodeObjectID
	sdb.WriteRelationship(ctx, "agent:agent-acme", "owner", "user:"+enc("alice"))
	sdb.WriteRelationship(ctx, "agent:agent-acme", "org", "organization:"+enc("acme"))
	sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "member", "user:"+enc("alice"))
	sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "admin", "user:"+enc("carol"))
	sdb.WriteRelationship(ctx, "organization:"+enc("acme"), "member", "user:"+enc("dave"))
	sdb.WriteRelationship(ctx, "organization:"+enc("globex"), "member", "user:"+enc("eve"))
}

// TestCanViewCanManage_Matrix is the Phase-4 authorization matrix at the
// orchestrator tier (view/manage against SpiceDB for org agents, equality for
// org-less), mirroring the registry matrix.
func TestCanViewCanManage_Matrix(t *testing.T) {
	reg := orgAuthzRegistry(t)
	defer reg.Close()
	regClient := registry.New(reg.URL)
	sdb := spicedb.NewMock()
	seedOrgTuples(sdb)
	ctx := context.Background()

	cases := []struct {
		name              string
		agent, user       string
		wantView, wantMng bool
	}{
		{"owner+member", "agent-acme", "alice", true, true},
		{"org admin (not owner)", "agent-acme", "carol", true, true},
		{"other member: read-only", "agent-acme", "dave", true, false},
		{"cross-org: invisible", "agent-acme", "eve", false, false},
		{"empty user", "agent-acme", "", false, false},
		{"org-less owner", "agent-less", "frank", true, true},
		{"org-less non-owner", "agent-less", "dave", false, false},
		{"unknown agent", "agent-nope", "alice", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := canView(ctx, regClient, sdb, c.agent, c.user); got != c.wantView {
				t.Errorf("canView = %v, want %v", got, c.wantView)
			}
			if got := canManage(ctx, regClient, sdb, c.agent, c.user); got != c.wantMng {
				t.Errorf("canManage = %v, want %v", got, c.wantMng)
			}
		})
	}
}

// --- /spawn human-path claim derivation + enforceOrgSpawn seam ---------------

// spawnWithClaims drives the human /spawn path with the given claims and returns
// the response recorder plus the SpawnRequest the registry saw at preregister
// (captured OrgID/TenantID/UserID), so a test can assert what was derived.
func spawnWithClaims(t *testing.T, claims tokenvalidator.Claims, body string) (*httptest.ResponseRecorder, registry.AgentRecord) {
	t.Helper()
	var preregistered registry.AgentRecord
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/preregister":
			json.NewDecoder(r.Body).Decode(&preregistered)
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			// requiresTenant=true worker so we can also assert the tenant gate is
			// untouched; tests that don't want the gate pass a tenantId in the body.
			w.Write([]byte(`{"agentType":"worker","version":"1.0.0","status":"active","requiresTenant":true,"runtimeSpec":{"lifecycle":"short-lived"},"authzTemplate":{"spiceDbRelations":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer reg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), spicedb.NewMock(), reg.URL,
		fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")

	req := httptest.NewRequest("POST", "/spawn", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer human-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, preregistered
}

func humanOrgClaims(user, org string, roles []string, scopes ...string) tokenvalidator.Claims {
	return tokenvalidator.Claims{User: user, OrgID: org, Roles: roles, Audience: []string{"orchestrator"}, Scopes: scopes}
}

// TestSpawnHumanPath_OrgDerivedFromClaim: the spawned record's OrgID comes from
// the token's organization claim (never the body), while tenantId still comes
// from the body and requiresTenant is enforced — spec criterion 5.
func TestSpawnHumanPath_OrgDerivedFromClaim(t *testing.T) {
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:write")
	// Body tries to smuggle a different org via an orgId field AND supplies the
	// required tenantId (a genuine consumer primitive that must pass through).
	rec, pre := spawnWithClaims(t, claims, `{"agentType":"worker","orgId":"globex","tenantId":"cust-42"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("spawn: got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	if pre.OrgID != "acme" {
		t.Errorf("derived OrgID = %q, want acme (from the claim, never the body's globex)", pre.OrgID)
	}
	if pre.TenantID != "cust-42" {
		t.Errorf("TenantID = %q, want cust-42 (body primitive, untouched)", pre.TenantID)
	}
	if pre.UserID != "alice" {
		t.Errorf("UserID = %q, want alice", pre.UserID)
	}
}

// TestSpawnHumanPath_RequiresTenantUntouched: the requiresTenant gate still 400s
// when no tenantId is supplied, regardless of org derivation.
func TestSpawnHumanPath_RequiresTenantUntouched(t *testing.T) {
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:write")
	rec, _ := spawnWithClaims(t, claims, `{"agentType":"worker"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing tenantId: got %d, want 400 (requiresTenant gate)", rec.Code)
	}
}

// TestSpawnHumanPath_EnforceOrgSpawnSeam covers BOTH states of the package seam:
// OFF (Phase 4) org-less non-admin spawns as today; ON (Phase 5) org-less
// non-admin is 403 while a realm admin keeps the org-less path.
func TestSpawnHumanPath_EnforceOrgSpawnSeam(t *testing.T) {
	nonAdmin := humanOrgClaims("frank", "", nil, "orchestrator:write")
	admin := humanOrgClaims("alice", "", []string{"admin"}, "orchestrator:write")
	body := `{"agentType":"worker","tenantId":"cust-1"}`

	// Seam OFF (default): org-less non-admin spawn WORKS.
	if enforceOrgSpawn {
		t.Fatal("enforceOrgSpawn must default false in Phase 4")
	}
	if rec, pre := spawnWithClaims(t, nonAdmin, body); rec.Code != http.StatusAccepted || pre.OrgID != "" {
		t.Fatalf("seam OFF: org-less non-admin should spawn org-less, got %d orgID=%q", rec.Code, pre.OrgID)
	}

	// Seam ON (Phase 5 behaviour): org-less non-admin → 403; admin still works.
	enforceOrgSpawn = true
	defer func() { enforceOrgSpawn = false }()

	if rec, _ := spawnWithClaims(t, nonAdmin, body); rec.Code != http.StatusForbidden {
		t.Errorf("seam ON: org-less non-admin: got %d, want 403", rec.Code)
	}
	if rec, pre := spawnWithClaims(t, admin, body); rec.Code != http.StatusAccepted || pre.OrgID != "" {
		t.Errorf("seam ON: realm admin org-less spawn must still work, got %d orgID=%q", rec.Code, pre.OrgID)
	}
}

// --- list org visibility + per-agent canManage ------------------------------

// TestListAgents_OrgVisibilityAndCanManage exercises the list matrix: a caller
// sees every agent in their active org PLUS their own org-less agents, with a
// per-agent canManage that is true for owners and org admins only.
func TestListAgents_OrgVisibilityAndCanManage(t *testing.T) {
	all := []registry.AgentRecord{
		{AgentID: "acme-alice", UserID: "alice", OrgID: "acme"},
		{AgentID: "acme-bob", UserID: "bob", OrgID: "acme"},
		{AgentID: "globex-x", UserID: "zed", OrgID: "globex"},
		{AgentID: "less-dave", UserID: "dave", OrgID: ""},
		{AgentID: "less-other", UserID: "erin", OrgID: ""},
	}
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(all)
			return
		}
		http.NotFound(w, r)
	}))
	defer reg.Close()

	type item struct {
		AgentID   string `json:"agentId"`
		CanManage bool   `json:"canManage"`
	}
	// list runs as `user` with active org `org` and a SpiceDB role of "member",
	// "admin", or "" (claim names the org but NO membership tuple — a stale token).
	list := func(user, org, role string) map[string]bool {
		sdb := spicedb.NewMock()
		enc := spicedb.EncodeObjectID
		if role != "" {
			sdb.WriteRelationship(context.Background(), "organization:"+enc(org), role, "user:"+enc(user))
		}
		fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
		mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, reg.URL,
			fakeValidator{claims: humanOrgClaims(user, org, nil, "orchestrator:read")},
			"orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")
		req := httptest.NewRequest("GET", "/v1/agents", nil)
		req.Header.Set("Authorization", "Bearer t")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("list: got %d", rr.Code)
		}
		var items []item
		if err := json.NewDecoder(rr.Body).Decode(&items); err != nil {
			t.Fatalf("decode: %v", err)
		}
		got := map[string]bool{}
		for _, it := range items {
			got[it.AgentID] = it.CanManage
		}
		return got
	}

	// bob: SpiceDB member of acme. Sees BOTH acme agents; manages only his own.
	// Does NOT see globex or other users' org-less agents.
	bob := list("bob", "acme", "member")
	if len(bob) != 2 {
		t.Fatalf("bob should see exactly the 2 acme agents, got %v", bob)
	}
	if !bob["acme-bob"] || bob["acme-alice"] {
		t.Errorf("bob canManage: want own acme-bob only, got %v", bob)
	}
	if _, ok := bob["globex-x"]; ok {
		t.Error("bob must not see cross-org globex agent")
	}
	if _, ok := bob["less-dave"]; ok {
		t.Error("bob must not see another user's org-less agent")
	}

	// alice as org admin of acme: sees both acme agents and manages BOTH.
	aliceAdmin := list("alice", "acme", "admin")
	if !aliceAdmin["acme-alice"] || !aliceAdmin["acme-bob"] {
		t.Errorf("org admin must manage every acme agent, got %v", aliceAdmin)
	}

	// STALE TOKEN: mallory's token still names acme but SpiceDB has NO membership
	// tuple (she was removed). She must see NO acme agents — visibility is gated on
	// SpiceDB, not the bare claim. She owns none, so the list is empty.
	stale := list("mallory", "acme", "")
	if len(stale) != 0 {
		t.Errorf("removed member with a stale org claim must see no org agents, got %v", stale)
	}

	// REMOVED OWNER: bob's membership tuple is gone (role="") but he still OWNS
	// acme-bob (owner tuple survives member removal). He must still see & manage
	// his OWN org agent — consistent with canView/canManage, which succeed on it by
	// direct id — while NOT seeing alice's acme-alice (no membership).
	removedOwner := list("bob", "acme", "")
	if len(removedOwner) != 1 || !removedOwner["acme-bob"] {
		t.Errorf("removed owner should still see & manage only their own org agent, got %v", removedOwner)
	}

	// dave org-less (no active org): sees ONLY his own org-less agent, manages it.
	dave := list("dave", "", "")
	if len(dave) != 1 || !dave["less-dave"] {
		t.Errorf("org-less dave should see & manage only less-dave, got %v", dave)
	}
}
