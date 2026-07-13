package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fakeclient "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

func adminClaims(user string, scopes ...string) tokenvalidator.Claims {
	c := humanClaims(user, scopes...)
	c.Roles = []string{"admin"}
	return c
}

// orgForwardMux wires an orchestrator whose registry is a stub recording the
// forwarded method+path (and echoing a marker body), so the admin org routes can
// be checked for gating + faithful forwarding.
func orgForwardMux(t *testing.T, claims tokenvalidator.Claims) (http.Handler, *[]string) {
	t.Helper()
	var seen []string
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(reg.Close)
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), spicedb.NewMock(), reg.URL,
		fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")
	return mux, &seen
}

// TestSignupOrg_DerivesFirstAdminFromToken proves the self-serve signup route
// (Phase 7): it is requireToken-gated (NOT admin), forwards to the registry's
// POST /v1/orgs choke point with ?selfServe=1, and stamps the FIRST ADMIN from
// the validated token sub — never a body field. A body trying to name a
// different adminUser is ignored.
func TestSignupOrg_DerivesFirstAdminFromToken(t *testing.T) {
	var gotURI, gotBody string
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.Method + " " + r.URL.RequestURI()
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"orgId":"acme"}`)
	}))
	t.Cleanup(reg.Close)
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	// newbie is a plain (non-admin), org-less user — the signup caller.
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), spicedb.NewMock(), reg.URL,
		fakeValidator{claims: humanClaims("newbie", "orchestrator:write")},
		"orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")

	// The body tries to smuggle a different adminUser — it must be ignored.
	req := httptest.NewRequest("POST", "/v1/signup/org",
		strings.NewReader(`{"orgId":"acme","name":"Acme","adminUser":"attacker"}`))
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("signup status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if gotURI != "POST /v1/orgs?selfServe=1" {
		t.Errorf("forwarded URI = %q, want POST /v1/orgs?selfServe=1", gotURI)
	}
	if !strings.Contains(gotBody, `"adminUser":"newbie"`) {
		t.Errorf("forwarded body = %q, want adminUser=newbie (token sub)", gotBody)
	}
	if strings.Contains(gotBody, "attacker") {
		t.Errorf("forwarded body must not carry the body-supplied adminUser; got %q", gotBody)
	}
}

func TestSignupOrg_RequiresToken(t *testing.T) {
	mux, seen := orgForwardMux(t, humanClaims("newbie", "orchestrator:write"))
	// No Authorization header ⇒ unauthorized, registry untouched.
	req := httptest.NewRequest("POST", "/v1/signup/org", strings.NewReader(`{"orgId":"acme"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated signup status = %d, want 401", rec.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("registry must not be called without a token; saw %v", *seen)
	}
}

func TestAdminOrgs_NonAdminForbidden(t *testing.T) {
	mux, seen := orgForwardMux(t, humanClaims("viewer", "orchestrator:write")) // no admin role
	req := httptest.NewRequest("POST", "/v1/admin/orgs", strings.NewReader(`{"orgId":"acme","adminUser":"alice"}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", rec.Code)
	}
	if len(*seen) != 0 {
		t.Fatalf("registry should not be called for a forbidden request; saw %v", *seen)
	}
}

func TestAdminOrgs_AdminForwards(t *testing.T) {
	mux, seen := orgForwardMux(t, adminClaims("alice", "orchestrator:write", "orchestrator:read"))
	cases := []struct{ method, path, want string }{
		{"POST", "/v1/admin/orgs", "POST /v1/orgs"},
		{"GET", "/v1/admin/orgs", "GET /v1/orgs"},
		{"GET", "/v1/admin/orgs?member=alice", "GET /v1/orgs?member=alice"},
		{"GET", "/v1/admin/orgs/acme", "GET /v1/orgs/acme"},
		{"PATCH", "/v1/admin/orgs/acme", "PATCH /v1/orgs/acme"},
		{"DELETE", "/v1/admin/orgs/acme", "DELETE /v1/orgs/acme"},
		{"GET", "/v1/admin/orgs/acme/members", "GET /v1/orgs/acme/members"},
		{"POST", "/v1/admin/orgs/acme/members", "POST /v1/orgs/acme/members"},
		{"PATCH", "/v1/admin/orgs/acme/members/bob", "PATCH /v1/orgs/acme/members/bob"},
		{"DELETE", "/v1/admin/orgs/acme/members/bob", "DELETE /v1/orgs/acme/members/bob"},
	}
	for _, tc := range cases {
		*seen = nil
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("%s %s: status=%d, want 201", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 1 || (*seen)[0] != tc.want {
			t.Errorf("%s %s: forwarded %v, want %q", tc.method, tc.path, *seen, tc.want)
		}
	}
}

// TestAdminUsers_SelfTargetForbidden proves the caller≠target guard (account-
// lifecycle follow-up, item B): an admin cannot disable or delete THEIR OWN
// account via the API (the UI hides the button; this closes the direct-curl
// path). The registry is never touched. The match is case-insensitive, mirroring
// the registry's isMasterAdmin EqualFold, so a mixed-case self target can't slip
// past. Enable is intentionally NOT guarded.
func TestAdminUsers_SelfTargetForbidden(t *testing.T) {
	cases := []struct{ method, path string }{
		{"POST", "/v1/admin/users/alice/disable"},
		{"DELETE", "/v1/admin/users/alice"},
		// Keycloak lowercases usernames; a mixed-case path must still be caught.
		{"POST", "/v1/admin/users/Alice/disable"},
		{"DELETE", "/v1/admin/users/ALICE"},
	}
	for _, tc := range cases {
		mux, seen := orgForwardMux(t, adminClaims("alice", "orchestrator:write", "orchestrator:read"))
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: self-target status = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 0 {
			t.Errorf("%s %s: registry must not be called for a self-target; saw %v", tc.method, tc.path, *seen)
		}
	}
}

// TestAdminUsers_SelfEnableAllowed proves ENABLE is intentionally unguarded: an
// admin re-enabling their own account is harmless and still forwards.
func TestAdminUsers_SelfEnableAllowed(t *testing.T) {
	mux, seen := orgForwardMux(t, adminClaims("alice", "orchestrator:write", "orchestrator:read"))
	req := httptest.NewRequest("POST", "/v1/admin/users/alice/enable", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("self-enable status = %d, want 201 (forwarded)", rec.Code)
	}
	if len(*seen) != 1 || (*seen)[0] != "POST /v1/users/alice/enable" {
		t.Errorf("self-enable forwarded %v, want POST /v1/users/alice/enable", *seen)
	}
}

// TestAdminUsers_NonAdminForbidden proves the master-admin user lifecycle routes
// (signup-lifecycle Phase 3) are requireAdmin-gated at the orchestrator: a
// non-admin token is 403'd and the registry is never touched.
func TestAdminUsers_NonAdminForbidden(t *testing.T) {
	cases := []struct{ method, path string }{
		{"GET", "/v1/admin/users"},
		{"POST", "/v1/admin/users/bob/disable"},
		{"POST", "/v1/admin/users/bob/enable"},
		{"DELETE", "/v1/admin/users/bob"},
	}
	for _, tc := range cases {
		mux, seen := orgForwardMux(t, humanClaims("viewer", "orchestrator:write", "orchestrator:read")) // no admin role
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer t")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s: non-admin status = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 0 {
			t.Errorf("%s %s: registry must not be called for a forbidden request; saw %v", tc.method, tc.path, *seen)
		}
	}
}

// TestAdminUsers_AdminForwards proves each user-lifecycle admin route forwards to
// the correct registry /v1/users path with the control-plane bearer.
func TestAdminUsers_AdminForwards(t *testing.T) {
	mux, seen := orgForwardMux(t, adminClaims("alice", "orchestrator:write", "orchestrator:read"))
	cases := []struct{ method, path, want string }{
		{"GET", "/v1/admin/users", "GET /v1/users"},
		{"POST", "/v1/admin/users/bob/disable", "POST /v1/users/bob/disable"},
		{"POST", "/v1/admin/users/bob/enable", "POST /v1/users/bob/enable"},
		{"DELETE", "/v1/admin/users/bob", "DELETE /v1/users/bob"},
	}
	for _, tc := range cases {
		*seen = nil
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("%s %s: status=%d, want 201", tc.method, tc.path, rec.Code)
		}
		if len(*seen) != 1 || (*seen)[0] != tc.want {
			t.Errorf("%s %s: forwarded %v, want %q", tc.method, tc.path, *seen, tc.want)
		}
	}
}
