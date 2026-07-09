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
		// tenant-visibility Phase 2: a plain member no longer views a non-owned org
		// agent — view == manage == owner + org admin.
		{"other member: no view (own-only)", "agent-acme", "dave", false, false},
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
// (captured OrgID/TenantID/UserID). When the claims name an org it SEEDS the
// caller's SpiceDB member tuple first — the realistic state (Keycloak only issues
// an org claim to a member, and reseed-orgs writes the matching tuple), so the
// human-path subject-consistency guard passes. Use spawnWithClaimsSDB to drive the
// guard's negative path (org claim but no membership tuple).
func spawnWithClaims(t *testing.T, claims tokenvalidator.Claims, body string) (*httptest.ResponseRecorder, registry.AgentRecord) {
	t.Helper()
	sdb := spicedb.NewMock()
	if claims.OrgID != "" {
		enc := spicedb.EncodeObjectID
		user := strings.TrimPrefix(claims.User, "user:")
		sdb.WriteRelationship(context.Background(), "organization:"+enc(claims.OrgID), "member", "user:"+enc(user))
	}
	return spawnWithClaimsSDB(t, claims, body, sdb)
}

// spawnWithClaimsSDB is spawnWithClaims with an explicit SpiceDB mock, so a test
// can control whether the caller's org-membership tuple exists (the guard path).
func spawnWithClaimsSDB(t *testing.T, claims tokenvalidator.Claims, body string, sdb spicedb.Client) (*httptest.ResponseRecorder, registry.AgentRecord) {
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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, reg.URL,
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

// TestSpawnHumanPath_OrgEnforcement pins the Phase-5 enforced state (the seam is
// gone): an org-less NON-admin human is refused (403 "no active organization"),
// while a realm admin keeps the org-less (global) spawn path.
func TestSpawnHumanPath_OrgEnforcement(t *testing.T) {
	nonAdmin := humanOrgClaims("frank", "", nil, "orchestrator:write")
	admin := humanOrgClaims("alice", "", []string{"admin"}, "orchestrator:write")
	body := `{"agentType":"worker","tenantId":"cust-1"}`

	if rec, _ := spawnWithClaims(t, nonAdmin, body); rec.Code != http.StatusForbidden {
		t.Errorf("org-less non-admin: got %d, want 403", rec.Code)
	}
	if rec, pre := spawnWithClaims(t, admin, body); rec.Code != http.StatusAccepted || pre.OrgID != "" {
		t.Errorf("realm admin org-less spawn must still work, got %d orgID=%q", rec.Code, pre.OrgID)
	}
}

// TestSpawnHumanPath_MembershipGuard proves the subject-consistency guard: a token
// that CARRIES an org claim but whose sub does NOT resolve to a SpiceDB membership
// tuple (the "future UUID-sub login path silently breaks org authz" scenario, or a
// removed member on a stale claim) is refused 403 rather than spawning into an org
// the authz layer can't police. spawnWithClaimsSDB uses an UNSEEDED mock, so the
// member check fails.
func TestSpawnHumanPath_MembershipGuard(t *testing.T) {
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:write")
	rec, pre := spawnWithClaimsSDB(t, claims, `{"agentType":"worker","tenantId":"cust-1"}`, spicedb.NewMock())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("org claim without a SpiceDB membership tuple must be refused, got %d (body: %s)", rec.Code, rec.Body.String())
	}
	if pre.AgentID != "" {
		t.Errorf("nothing should have been preregistered on a guard failure, got %+v", pre)
	}
}

// --- GET /v1/my/orgs (org picker / no-org screen source) --------------------

// myOrgsResp mirrors the orchestrator's GET /v1/my/orgs response shape.
type myOrgsResp struct {
	Orgs       []map[string]any `json:"orgs"`
	ActiveOrg  string           `json:"activeOrg"`
	IsOrgAdmin bool             `json:"isOrgAdmin"`
}

// TestMyOrgs verifies the session-gated my-orgs endpoint: it keys the registry
// membership lookup on the TOKEN SUB (never a client param), surfaces the active
// org from the claim, and resolves isOrgAdmin from SpiceDB for that active org.
func TestMyOrgs(t *testing.T) {
	// Registry stub: only alice is a member of acme; asserts the orchestrator asked
	// for member=<sub> and nothing else.
	var sawMember string
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/orgs" {
			http.NotFound(w, r)
			return
		}
		sawMember = r.URL.Query().Get("member")
		w.Header().Set("Content-Type", "application/json")
		if sawMember == "alice" {
			w.Write([]byte(`[{"orgId":"acme","name":"Acme"}]`))
		} else {
			w.Write([]byte(`[]`))
		}
	}))
	defer reg.Close()

	call := func(user, org, role string) (int, myOrgsResp) {
		sdb := spicedb.NewMock()
		enc := spicedb.EncodeObjectID
		if role != "" {
			sdb.WriteRelationship(context.Background(), "organization:"+enc(org), role, "user:"+enc(user))
		}
		fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
		mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, reg.URL,
			fakeValidator{claims: humanOrgClaims(user, org, nil, "orchestrator:read")},
			"orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")
		req := httptest.NewRequest("GET", "/v1/my/orgs", nil)
		req.Header.Set("Authorization", "Bearer t")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		var out myOrgsResp
		json.NewDecoder(rr.Body).Decode(&out)
		return rr.Code, out
	}

	// alice, active org acme, admin tuple → 1 org, activeOrg acme, isOrgAdmin true.
	code, out := call("alice", "acme", "admin")
	if code != http.StatusOK {
		t.Fatalf("my/orgs: got %d", code)
	}
	if sawMember != "alice" {
		t.Errorf("registry member param = %q, want the token sub alice", sawMember)
	}
	if len(out.Orgs) != 1 || out.Orgs[0]["orgId"] != "acme" {
		t.Errorf("orgs = %v, want [acme]", out.Orgs)
	}
	if out.ActiveOrg != "acme" {
		t.Errorf("activeOrg = %q, want acme", out.ActiveOrg)
	}
	if !out.IsOrgAdmin {
		t.Error("isOrgAdmin should be true for the org admin")
	}

	// viewer as a plain member: isOrgAdmin false. (Registry stub returns [] for a
	// non-alice member, which is fine — the assertion is on isOrgAdmin.)
	if _, out := call("viewer", "acme", "member"); out.IsOrgAdmin {
		t.Error("isOrgAdmin should be false for a plain member")
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

	// bob: SpiceDB member (NOT admin) of acme. tenant-visibility Phase 2: a plain
	// member sees ONLY the agents they own — so bob sees his own acme-bob and NOT
	// alice's acme-alice. He manages his own. He never sees globex or other users'
	// org-less agents.
	bob := list("bob", "acme", "member")
	if len(bob) != 1 {
		t.Fatalf("bob (member) should see exactly his own acme agent, got %v", bob)
	}
	if !bob["acme-bob"] {
		t.Errorf("bob canManage: want own acme-bob only, got %v", bob)
	}
	if _, ok := bob["acme-alice"]; ok {
		t.Error("bob (plain member) must NOT see another member's org agent (Phase 2)")
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

// TestListAgents_IsolationInvariant is the security core (spec criterion 7): a
// NON-realm-admin caller CANNOT widen their scope by supplying an org to the
// orchestrator. The active org is taken ONLY from the token's `organization`
// claim (orgIDFrom); a client-supplied `?org=<other>` query is ignored, so the
// response is byte-identical to omitting it. Today the handler never reads
// `?org=` at all — this test LOCKS that and becomes the permanent guard once
// Phase 3 introduces the realm-admin-only viewing-org param (which must remain
// asymmetric: honored for a realm admin, ignored for everyone else).
func TestListAgents_IsolationInvariant(t *testing.T) {
	all := []registry.AgentRecord{
		{AgentID: "acme-bob", UserID: "bob", OrgID: "acme"},
		{AgentID: "acme-alice", UserID: "alice", OrgID: "acme"},
		{AgentID: "globex-x", UserID: "zed", OrgID: "globex"},
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

	// bob is a plain MEMBER of acme (a non-realm-admin). He owns acme-bob.
	sdb := spicedb.NewMock()
	enc := spicedb.EncodeObjectID
	sdb.WriteRelationship(context.Background(), "organization:"+enc("acme"), "member", "user:"+enc("bob"))
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, reg.URL,
		fakeValidator{claims: humanOrgClaims("bob", "acme", nil, "orchestrator:read")},
		"orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")

	get := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		req.Header.Set("Authorization", "Bearer t")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("list %s: got %d", target, rr.Code)
		}
		return rr.Body.String()
	}

	base := get("/v1/agents")
	// A forged/extra org param naming ANOTHER org must NOT widen scope.
	widened := get("/v1/agents?org=globex")
	if base != widened {
		t.Errorf("non-admin ?org= must be ignored (org from token claim only)\n base    = %s\n ?org=   = %s", base, widened)
	}
	// Sanity: bob (member) sees ONLY his own agent — never acme-alice or globex-x.
	if !strings.Contains(base, "acme-bob") || strings.Contains(base, "acme-alice") || strings.Contains(base, "globex-x") {
		t.Errorf("member list must be own-only, got %s", base)
	}
}
