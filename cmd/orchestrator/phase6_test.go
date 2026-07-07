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

// phase6Mux wires an orchestrator whose registry stub records the forwarded
// method+request-URI (so gate/forward behaviour can be asserted) and echoes a
// marker body. The caller supplies pre-seeded SpiceDB tuples.
func phase6Mux(t *testing.T, claims tokenvalidator.Claims, sdb spicedb.Client) (http.Handler, *[]string) {
	t.Helper()
	var seen []string
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(reg.Close)
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, reg.URL,
		fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")
	return mux, &seen
}

// seedMembership writes an org membership tuple (role member|admin) as production would.
func seedMembership(sdb *spicedb.Mock, org, role, user string) {
	enc := spicedb.EncodeObjectID
	sdb.WriteRelationship(context.Background(), "organization:"+enc(org), role, "user:"+enc(user))
}

func doReq(t *testing.T, mux http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// --- Org self-service authz (requireOrgAdmin / requireOrgMember) --------------

func TestOrgSelfService_AdminManagesOwnOrg(t *testing.T) {
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "admin", "alice")
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)

	cases := []struct{ method, path, want string }{
		{"PATCH", "/v1/orgs/acme", "PATCH /v1/orgs/acme"},
		{"GET", "/v1/orgs/acme", "GET /v1/orgs/acme"},
		{"GET", "/v1/orgs/acme/members", "GET /v1/orgs/acme/members"},
		{"POST", "/v1/orgs/acme/members", "POST /v1/orgs/acme/members"},
		{"PATCH", "/v1/orgs/acme/members/bob", "PATCH /v1/orgs/acme/members/bob"},
		{"DELETE", "/v1/orgs/acme/members/bob", "DELETE /v1/orgs/acme/members/bob"},
	}
	for _, tc := range cases {
		*seen = nil
		rec := doReq(t, mux, tc.method, tc.path, `{}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("%s %s: status=%d, want 201 (body %s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		if len(*seen) != 1 || (*seen)[0] != tc.want {
			t.Errorf("%s %s: forwarded %v, want %q", tc.method, tc.path, *seen, tc.want)
		}
	}
}

func TestOrgSelfService_MemberReadOnly(t *testing.T) {
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "member", "viewer")
	claims := humanOrgClaims("viewer", "acme", nil, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)

	// A member CAN read the member list.
	*seen = nil
	if rec := doReq(t, mux, "GET", "/v1/orgs/acme/members", ``); rec.Code != http.StatusCreated {
		t.Errorf("member GET members: status=%d, want 201-forward", rec.Code)
	}
	if len(*seen) != 1 {
		t.Errorf("member GET members should forward, saw %v", *seen)
	}

	// A member CANNOT mutate — every admin route is a 404 (no oracle), no forward.
	for _, tc := range []struct{ method, path string }{
		{"PATCH", "/v1/orgs/acme"},
		{"POST", "/v1/orgs/acme/members"},
		{"PATCH", "/v1/orgs/acme/members/bob"},
		{"DELETE", "/v1/orgs/acme/members/bob"},
	} {
		*seen = nil
		rec := doReq(t, mux, tc.method, tc.path, `{}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("member %s %s: status=%d, want 404", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 0 {
			t.Errorf("member %s %s must not forward, saw %v", tc.method, tc.path, *seen)
		}
	}
}

func TestOrgSelfService_CrossOrg404(t *testing.T) {
	// alice is an admin of acme, but her token is scoped to acme; a request for
	// globex must 404 (claim mismatch), never reaching the registry.
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "admin", "alice")
	seedMembership(sdb, "globex", "admin", "alice") // even if she's admin there, the single-org token can't act
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)

	for _, tc := range []struct{ method, path string }{
		{"PATCH", "/v1/orgs/globex"},
		{"GET", "/v1/orgs/globex/members"},
		{"POST", "/v1/orgs/globex/members"},
	} {
		*seen = nil
		rec := doReq(t, mux, tc.method, tc.path, `{}`)
		if rec.Code != http.StatusNotFound {
			t.Errorf("cross-org %s %s: status=%d, want 404", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 0 {
			t.Errorf("cross-org %s %s must not forward, saw %v", tc.method, tc.path, *seen)
		}
	}
}

func TestOrgSelfService_RealmAdminBypass(t *testing.T) {
	// A realm admin (role=admin, org-less token) manages ANY org's self-service
	// routes without a membership tuple.
	sdb := spicedb.NewMock()
	claims := humanOrgClaims("root", "", []string{"admin"}, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)
	*seen = nil
	if rec := doReq(t, mux, "POST", "/v1/orgs/globex/members", `{"user":"x"}`); rec.Code != http.StatusCreated {
		t.Errorf("realm admin POST members: status=%d, want 201-forward (body %s)", rec.Code, rec.Body.String())
	}
	if len(*seen) != 1 || (*seen)[0] != "POST /v1/orgs/globex/members" {
		t.Errorf("realm admin forward = %v", *seen)
	}
}

// --- Template management authz + org stamping ---------------------------------

func TestTemplateManage_OrgAdminStampsOrg(t *testing.T) {
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "admin", "alice")
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)

	cases := []struct{ method, path, want string }{
		{"POST", "/v1/templates", "POST /v1/templates?orgId=acme"},
		{"PATCH", "/v1/templates/foo", "PATCH /v1/templates/foo?orgId=acme"},
		{"DELETE", "/v1/templates/foo", "DELETE /v1/templates/foo?orgId=acme"},
	}
	for _, tc := range cases {
		*seen = nil
		rec := doReq(t, mux, tc.method, tc.path, `{"agentType":"foo"}`)
		if rec.Code != http.StatusCreated {
			t.Errorf("%s %s: status=%d, want forward", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 1 || (*seen)[0] != tc.want {
			t.Errorf("%s %s: forwarded %v, want %q", tc.method, tc.path, *seen, tc.want)
		}
	}
}

func TestTemplateManage_RealmAdminNoOrg(t *testing.T) {
	sdb := spicedb.NewMock()
	claims := humanOrgClaims("root", "", []string{"admin"}, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)
	*seen = nil
	if rec := doReq(t, mux, "POST", "/v1/templates", `{"agentType":"foo"}`); rec.Code != http.StatusCreated {
		t.Errorf("realm admin create: status=%d", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0] != "POST /v1/templates" {
		t.Errorf("realm admin create forwarded %v, want no orgId", *seen)
	}
}

func TestTemplateManage_MemberForbidden(t *testing.T) {
	// A plain member (not org admin, not realm admin) cannot manage templates.
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "member", "viewer")
	claims := humanOrgClaims("viewer", "acme", nil, "orchestrator:write", "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)
	*seen = nil
	if rec := doReq(t, mux, "POST", "/v1/templates", `{"agentType":"foo"}`); rec.Code != http.StatusForbidden {
		t.Errorf("member create: status=%d, want 403", rec.Code)
	}
	if len(*seen) != 0 {
		t.Errorf("member create must not forward, saw %v", *seen)
	}
}

// --- Template list forwarding carries the caller's org ------------------------

func TestTemplateList_ForwardsCallerOrg(t *testing.T) {
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "admin", "alice")
	claims := humanOrgClaims("alice", "acme", nil, "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)

	cases := []struct{ path, want string }{
		{"/v1/templates", "GET /v1/templates?orgId=acme"},
		{"/v1/templates/spawn", "GET /v1/templates?detail=spawn&orgId=acme"},
		{"/v1/org/templates", "GET /v1/templates?detail=full&orgId=acme"},
	}
	for _, tc := range cases {
		*seen = nil
		if rec := doReq(t, mux, "GET", tc.path, ``); rec.Code != http.StatusCreated {
			t.Errorf("GET %s: status=%d", tc.path, rec.Code)
		}
		if len(*seen) != 1 || (*seen)[0] != tc.want {
			t.Errorf("GET %s: forwarded %v, want %q", tc.path, *seen, tc.want)
		}
	}
}

func TestTemplateList_RealmAdminFullNoOrg(t *testing.T) {
	sdb := spicedb.NewMock()
	claims := humanOrgClaims("root", "", []string{"admin"}, "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)
	*seen = nil
	if rec := doReq(t, mux, "GET", "/v1/admin/templates", ``); rec.Code != http.StatusCreated {
		t.Errorf("admin templates: status=%d", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0] != "GET /v1/templates?detail=full" {
		t.Errorf("admin templates forwarded %v", *seen)
	}
}

func TestOrgTemplates_MemberForbidden(t *testing.T) {
	sdb := spicedb.NewMock()
	seedMembership(sdb, "acme", "member", "viewer")
	claims := humanOrgClaims("viewer", "acme", nil, "orchestrator:read")
	mux, seen := phase6Mux(t, claims, sdb)
	*seen = nil
	if rec := doReq(t, mux, "GET", "/v1/org/templates", ``); rec.Code != http.StatusForbidden {
		t.Errorf("member org/templates: status=%d, want 403", rec.Code)
	}
	if len(*seen) != 0 {
		t.Errorf("member org/templates must not forward, saw %v", *seen)
	}
}

// --- /spawn org-scoped template gate ------------------------------------------

// spawnTemplateOrg drives the human /spawn path where the target template carries
// tplOrg (empty = global). Returns the response recorder.
func spawnTemplateOrg(t *testing.T, claims tokenvalidator.Claims, tplOrg, body string) *httptest.ResponseRecorder {
	t.Helper()
	sdb := spicedb.NewMock()
	if claims.OrgID != "" {
		seedMembership(sdb, claims.OrgID, "member", strings.TrimPrefix(claims.User, "user:"))
	}
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/preregister":
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			tpl := registry.AgentTemplate{
				AgentType: "worker", Version: "1", Status: "active", OrgID: tplOrg,
				Runtime: registry.RuntimeSpec{Lifecycle: "short-lived"},
			}
			json.NewEncoder(w).Encode(tpl)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(reg.Close)
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, reg.URL,
		fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")
	req := httptest.NewRequest("POST", "/spawn", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer human-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestSpawnTemplateOrgGate(t *testing.T) {
	acme := humanOrgClaims("alice", "acme", nil, "orchestrator:write")
	globex := humanOrgClaims("bob", "globex", nil, "orchestrator:write")
	admin := humanOrgClaims("root", "", []string{"admin"}, "orchestrator:write")
	body := `{"agentType":"worker"}`

	// Matching org → spawns.
	if rec := spawnTemplateOrg(t, acme, "acme", body); rec.Code != http.StatusAccepted {
		t.Errorf("acme template in acme: got %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	// Global template → spawns in any org.
	if rec := spawnTemplateOrg(t, acme, "", body); rec.Code != http.StatusAccepted {
		t.Errorf("global template in acme: got %d, want 202", rec.Code)
	}
	// Global template → spawns for a realm admin (org-less).
	if rec := spawnTemplateOrg(t, admin, "", body); rec.Code != http.StatusAccepted {
		t.Errorf("global template, realm admin: got %d, want 202", rec.Code)
	}
	// Cross-org template → 400 "unknown agent type" (no oracle).
	rec := spawnTemplateOrg(t, globex, "acme", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("acme template in globex: got %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown agent type") {
		t.Errorf("cross-org spawn body = %q, want the same 'unknown agent type' as a missing template (no oracle)", rec.Body.String())
	}
}
