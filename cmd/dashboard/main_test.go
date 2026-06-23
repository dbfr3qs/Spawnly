package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestInjectUserID(t *testing.T) {
	// A browser-supplied userId is overwritten with the authenticated user.
	out, err := injectUserID([]byte(`{"agentType":"worker","userId":"evil","tenantId":"t1"}`), "alice")
	if err != nil {
		t.Fatalf("injectUserID: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["userId"] != "alice" {
		t.Errorf("userId = %v, want alice", m["userId"])
	}
	if m["agentType"] != "worker" || m["tenantId"] != "t1" {
		t.Errorf("other fields not preserved: %v", m)
	}

	// An empty body still yields a valid spawn body carrying the identity.
	out, err = injectUserID(nil, "alice")
	if err != nil {
		t.Fatalf("injectUserID(empty): %v", err)
	}
	m = nil
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if m["userId"] != "alice" {
		t.Errorf("userId = %v, want alice", m["userId"])
	}
}

func TestRequireUnauthenticated(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://identity-server:8080", "dashboard", "secret")
	h := a.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("next should not run without a session")
	}))

	// API calls get a 401.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/spawn", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/api/spawn status = %d, want 401", rec.Code)
	}

	// A real top-level page navigation is redirected to /signin (the OIDC
	// initiator; the browser then lands on the /login form).
	rec = httptest.NewRecorder()
	nav := httptest.NewRequest("GET", "/", nil)
	nav.Header.Set("Sec-Fetch-Dest", "document")
	h.ServeHTTP(rec, nav)
	if rec.Code != http.StatusFound {
		t.Errorf("navigation status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/signin" {
		t.Errorf("redirect = %q, want /signin", loc)
	}

	// A subresource request while logged out (e.g. the browser's eager favicon
	// fetch) must NOT redirect to /login — that would mint a second login state
	// and clobber the in-flight navigation's state cookie ("invalid state").
	rec = httptest.NewRecorder()
	fav := httptest.NewRequest("GET", "/favicon.ico", nil)
	fav.Header.Set("Sec-Fetch-Dest", "image")
	h.ServeHTTP(rec, fav)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("favicon status = %d, want 401 (no redirect)", rec.Code)
	}
}

func TestRequireAuthenticated(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://identity-server:8080", "dashboard", "secret")
	a.sessions["sid-1"] = session{username: "alice", expiresAt: time.Now().Add(time.Hour)}

	called := false
	h := a.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if u, ok := a.user(r); !ok || u != "alice" {
			t.Errorf("user() = %q,%v want alice,true", u, ok)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid-1"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !called {
		t.Error("next was not called for an authenticated request")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestExpiredSessionRejected(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://identity-server:8080", "dashboard", "secret")
	a.sessions["old"] = session{username: "alice", expiresAt: time.Now().Add(-time.Minute)}

	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "old"})
	if _, ok := a.user(req); ok {
		t.Error("expired session should not authenticate")
	}
}
