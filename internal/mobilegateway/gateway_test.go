package mobilegateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spawnly/platform/internal/tokenvalidator"
)

// mapValidator resolves a bearer token string to fixed claims, so a test can
// model several distinct users and an unknown/invalid token.
type mapValidator struct {
	byToken map[string]tokenvalidator.Claims
}

func (m mapValidator) ValidateAccessToken(_ context.Context, tok string) (tokenvalidator.Claims, error) {
	c, ok := m.byToken[tok]
	if !ok {
		return tokenvalidator.Claims{}, fmt.Errorf("unknown token")
	}
	return c, nil
}

func rwClaims(user string) tokenvalidator.Claims {
	return tokenvalidator.Claims{
		User:     user,
		Audience: []string{"orchestrator"},
		Scopes:   []string{"orchestrator:read", "orchestrator:write"},
	}
}

func testDeps(v tokenvalidator.TokenValidator, orchestratorURL string) Deps {
	return Deps{
		Validator:       v,
		Devices:         NewMemoryDeviceStore(),
		OrchestratorURL: orchestratorURL,
		Audience:        "orchestrator",
		ReadScope:       "orchestrator:read",
		WriteScope:      "orchestrator:write",
	}
}

func do(t *testing.T, mux http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestAuth_Rejections(t *testing.T) {
	v := mapValidator{byToken: map[string]tokenvalidator.Claims{
		"alice":     rwClaims("user:alice"),
		"wrong-aud": {User: "user:x", Audience: []string{"registry"}, Scopes: []string{"orchestrator:read", "orchestrator:write"}},
		"read-only": {User: "user:x", Audience: []string{"orchestrator"}, Scopes: []string{"orchestrator:read"}},
		"deleg":     {User: "user:x", Audience: []string{"orchestrator"}, Scopes: []string{"orchestrator:write"}, TokenUse: "delegation"},
		"agent-chain": {User: "user:x", Audience: []string{"orchestrator"}, Scopes: []string{"orchestrator:read", "orchestrator:write"},
			Chain: []string{"spiffe://spawnly/agent/abc"}},
	}}
	mux := BuildMux(testDeps(v, "http://unused"))

	cases := []struct {
		name, bearer string
		want         int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"unknown token", "nope", http.StatusUnauthorized},
		{"wrong audience", "wrong-aud", http.StatusUnauthorized},
		{"delegation token", "deleg", http.StatusUnauthorized},
		{"agent actor-chain token", "agent-chain", http.StatusUnauthorized}, // human-only edge
		{"missing write scope", "read-only", http.StatusForbidden},          // POST /me/devices needs write
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, mux, "POST", "/me/devices", c.bearer, `{"platform":"ios","pushToken":"t"}`)
			if w.Code != c.want {
				t.Fatalf("got %d, want %d", w.Code, c.want)
			}
		})
	}
}

func TestRegisterDevice_UserIDFromTokenOnly(t *testing.T) {
	v := mapValidator{byToken: map[string]tokenvalidator.Claims{
		"alice": rwClaims("user:alice"),
		"bob":   rwClaims("user:bob"),
	}}
	d := testDeps(v, "http://unused")
	mux := BuildMux(d)

	// Alice registers, smuggling a forged userId in the body — it must be ignored.
	w := do(t, mux, "POST", "/me/devices", "alice", `{"platform":"android","pushToken":"tok-a","userId":"bob"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register: got %d (%s)", w.Code, w.Body.String())
	}
	var created Device
	json.Unmarshal(w.Body.Bytes(), &created)

	// The device lands under alice (from the token), not bob (from the body).
	alice, _ := d.Devices.ListByUser(context.Background(), "alice")
	if len(alice) != 1 || alice[0].PushToken != "tok-a" {
		t.Fatalf("alice devices = %+v, want one tok-a", alice)
	}
	bob, _ := d.Devices.ListByUser(context.Background(), "bob")
	if len(bob) != 0 {
		t.Fatalf("bob has devices from a forged body userId: %+v", bob)
	}

	// Bob cannot delete alice's device id.
	w = do(t, mux, "DELETE", "/me/devices/"+created.ID, "bob", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("bob deleting alice's device: got %d, want 404", w.Code)
	}
	// Alice can.
	w = do(t, mux, "DELETE", "/me/devices/"+created.ID, "alice", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("alice deleting own device: got %d, want 204", w.Code)
	}
}

func TestRegisterDevice_BadInput(t *testing.T) {
	v := mapValidator{byToken: map[string]tokenvalidator.Claims{"alice": rwClaims("user:alice")}}
	mux := BuildMux(testDeps(v, "http://unused"))
	for _, body := range []string{
		`{"platform":"windows","pushToken":"t"}`, // bad platform
		`{"platform":"ios"}`,                     // no token
		`not json`,
	} {
		w := do(t, mux, "POST", "/me/devices", "alice", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400", body, w.Code)
		}
	}
}

func TestConsentProxy_ForwardsTokenQueryAndBody(t *testing.T) {
	var gotAuth, gotQuery, gotBody, gotPath string
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer orch.Close()

	v := mapValidator{byToken: map[string]tokenvalidator.Claims{"alice": rwClaims("user:alice")}}
	mux := BuildMux(testDeps(v, orch.URL))

	// Approve forwards path, body (scope narrowing), and the user's bearer.
	w := do(t, mux, "POST", "/me/consent-requests/req-123/approve", "alice", `{"scopes":["a:read"]}`)
	if w.Code != http.StatusOK || w.Body.String() != `{"ok":true}` {
		t.Fatalf("approve relay: got %d %q", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer alice" {
		t.Fatalf("forwarded auth = %q, want the user's bearer", gotAuth)
	}
	if gotPath != "/v1/consent-requests/req-123/approve" {
		t.Fatalf("forwarded path = %q", gotPath)
	}
	if gotBody != `{"scopes":["a:read"]}` {
		t.Fatalf("forwarded body = %q", gotBody)
	}

	// List forwards the inbound query (status=pending) through.
	do(t, mux, "GET", "/me/consent-requests?status=pending", "alice", "")
	if gotQuery != "status=pending" {
		t.Fatalf("forwarded query = %q, want status=pending", gotQuery)
	}
}

func TestGetConsentRequestByID_SelectsAndScopes(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The gateway must ask for the user-scoped pending list.
		if r.URL.Query().Get("status") != "pending" {
			t.Errorf("by-id fetch query = %q, want status=pending", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"id":"req-1","childType":"x"},{"id":"req-2","childType":"y"}]`))
	}))
	defer orch.Close()

	v := mapValidator{byToken: map[string]tokenvalidator.Claims{"alice": rwClaims("user:alice")}}
	mux := BuildMux(testDeps(v, orch.URL))

	w := do(t, mux, "GET", "/me/consent-requests/req-2", "alice", "")
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	var cr map[string]any
	json.Unmarshal(w.Body.Bytes(), &cr)
	if cr["id"] != "req-2" || cr["childType"] != "y" {
		t.Fatalf("got %+v, want the verbatim req-2 element", cr)
	}

	// An id not in the user's pending set is 404 (never theirs, or resolved).
	w = do(t, mux, "GET", "/me/consent-requests/req-999", "alice", "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d, want 404", w.Code)
	}
}
