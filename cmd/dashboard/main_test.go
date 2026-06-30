package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/oauth2"
)

// TestAgentOpTargetEscapesID asserts the per-agent target PathEscape's the id
// (so a crafted id can't smuggle a query parameter) and carries NO userId — the
// orchestrator now derives the user from the access token's sub.
func TestAgentOpTargetEscapesID(t *testing.T) {
	got := agentOpTarget("http://orch", `realId?userId=victim&x=`, "")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.RawQuery != "" {
		t.Fatalf("target must carry no query, got %q", u.RawQuery)
	}
	if u.Path != "/v1/agents/realId?userId=victim&x=" {
		t.Fatalf("crafted id did not stay (escaped) in the path segment: path=%q", u.Path)
	}
}

// TestLogsOpTargetCarriesParams asserts the logs target keeps the inbound logs
// params and PathEscape's the id, but carries no userId (token-derived now).
func TestLogsOpTargetCarriesParams(t *testing.T) {
	q := url.Values{}
	q.Set("container", "agent-sidecar")
	q.Set("tailLines", "100")

	got := logsOpTarget("http://orch", `realId?x=1`, q)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse %q: %v", got, err)
	}
	if u.Path != "/v1/agents/realId?x=1/logs" {
		t.Fatalf("crafted id did not stay (escaped) in the path: path=%q", u.Path)
	}
	if u.Query().Has("userId") {
		t.Fatalf("target must not carry userId, query=%q", u.RawQuery)
	}
	if u.Query().Get("container") != "agent-sidecar" || u.Query().Get("tailLines") != "100" {
		t.Fatalf("logs params not carried through: query=%q", u.RawQuery)
	}
}

// stubAuth returns an Authenticator wired with a no-op ensure() and the given
// oauth config, so token-handling can be exercised without real OIDC discovery.
func stubAuth(oauth *oauth2.Config) *Authenticator {
	a := NewAuthenticator("http://localhost:8090", "http://identity-server:8080", "dashboard", "secret")
	a.once.Do(func() {}) // mark ensure() done so it won't hit the network
	a.oauth = oauth
	return a
}

// TestOrchestratorTokenReturnsValidToken: an unexpired access token is returned
// as-is, with no refresh round-trip.
func TestOrchestratorTokenReturnsValidToken(t *testing.T) {
	oauth := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://refresh.invalid/token"}}
	a := stubAuth(oauth)
	a.sessions["sid"] = session{
		username:  "alice",
		tokenSrc:  oauth.TokenSource(context.Background(), &oauth2.Token{AccessToken: "at-1", Expiry: time.Now().Add(time.Hour)}),
		expiresAt: time.Now().Add(time.Hour),
	}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})

	got, err := a.orchestratorToken(req.Context(), req)
	if err != nil {
		t.Fatalf("orchestratorToken: %v", err)
	}
	if got != "at-1" {
		t.Errorf("token = %q, want at-1", got)
	}
}

// TestOrchestratorTokenRefreshes: an expired access token is refreshed via the
// refresh token, and the rotated token is persisted back into the session.
func TestOrchestratorTokenRefreshes(t *testing.T) {
	var refreshes int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes++
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-1" {
			t.Errorf("unexpected refresh request: %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"at-new","token_type":"Bearer","refresh_token":"rt-2","expires_in":3600}`))
	}))
	defer ts.Close()

	oauth := &oauth2.Config{
		ClientID:     "dashboard",
		ClientSecret: "secret",
		Endpoint:     oauth2.Endpoint{TokenURL: ts.URL + "/token"},
	}
	a := stubAuth(oauth)
	a.sessions["sid"] = session{
		username:  "alice",
		tokenSrc:  oauth.TokenSource(context.Background(), &oauth2.Token{AccessToken: "at-old", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Minute)}),
		expiresAt: time.Now().Add(time.Hour),
	}
	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})

	got, err := a.orchestratorToken(req.Context(), req)
	if err != nil {
		t.Fatalf("orchestratorToken: %v", err)
	}
	if got != "at-new" {
		t.Errorf("token = %q, want at-new", got)
	}
	// A second call must reuse the cached (refreshed) token — no second redeem of
	// the (now-rotated) refresh token.
	if got2, err := a.orchestratorToken(req.Context(), req); err != nil || got2 != "at-new" {
		t.Errorf("second call = %q,%v want at-new,nil", got2, err)
	}
	if refreshes != 1 {
		t.Errorf("refresh endpoint hit %d times, want 1 (ReuseTokenSource dedupe)", refreshes)
	}
}

// TestOrchestratorTokenNoSession: without a session, fail closed (no token).
func TestOrchestratorTokenNoSession(t *testing.T) {
	a := stubAuth(&oauth2.Config{})
	req := httptest.NewRequest("GET", "/api/agents", nil)
	if _, err := a.orchestratorToken(req.Context(), req); err == nil {
		t.Fatal("expected an error with no session")
	}
}

// TestProxyAttachesBearerNoUserID drives a real /api/agents request through the
// mux and asserts the orchestrator-bound request carries Authorization: Bearer
// and NO userId query param (and no browser cookie leak upstream).
func TestProxyAttachesBearerNoUserID(t *testing.T) {
	var gotAuth, gotCookie, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotQuery = r.URL.RawQuery
		w.Write([]byte("[]"))
	}))
	defer upstream.Close()

	oauth := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://refresh.invalid/token"}}
	a := stubAuth(oauth)
	a.sessions["sid"] = session{
		username:  "alice",
		tokenSrc:  oauth.TokenSource(context.Background(), &oauth2.Token{AccessToken: "at-1", Expiry: time.Now().Add(time.Hour)}),
		expiresAt: time.Now().Add(time.Hour),
	}
	mux := buildMux(a, upstream.URL, "http://identity-server:8080", "http://docs", fstest.MapFS{})

	req := httptest.NewRequest("GET", "/api/agents", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if gotAuth != "Bearer at-1" {
		t.Errorf("upstream Authorization = %q, want \"Bearer at-1\"", gotAuth)
	}
	if gotQuery != "" {
		t.Errorf("upstream query = %q, want empty (no userId)", gotQuery)
	}
	if gotCookie != "" {
		t.Errorf("browser cookie leaked upstream: %q", gotCookie)
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

// TestTemplateRoutesRequireAdminAtBFF proves the dashboard BFF's template
// POST/PATCH/DELETE routes are actually wired through auth.requireAdmin in
// buildMux (not just the wrapper): a non-admin session gets 403 on each, AND
// the upstream orchestrator is never hit (the proxy would forward if the gate
// were bypassed). GET /api/templates stays on auth.require and is allowed for a
// non-admin (the spawn dropdown is unchanged). This is the BFF-tier twin of the
// orchestrator's TestTemplateManagementRequiresAdmin.
func TestTemplateRoutesRequireAdminAtBFF(t *testing.T) {
	var upstreamHits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	oauth := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://refresh.invalid/token"}}
	a := stubAuth(oauth)
	a.sessions["admin-sid"] = session{
		username: "alice", isAdmin: true,
		tokenSrc:  oauth.TokenSource(context.Background(), &oauth2.Token{AccessToken: "at-1", Expiry: time.Now().Add(time.Hour)}),
		expiresAt: time.Now().Add(time.Hour),
	}
	a.sessions["viewer-sid"] = session{
		username: "viewer", isAdmin: false,
		tokenSrc:  oauth.TokenSource(context.Background(), &oauth2.Token{AccessToken: "at-1", Expiry: time.Now().Add(time.Hour)}),
		expiresAt: time.Now().Add(time.Hour),
	}
	mux := buildMux(a, upstream.URL, "http://identity-server:8080", "http://docs", fstest.MapFS{})

	mutateCases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/templates"},
		{http.MethodPatch, "/api/templates/worker"},
		{http.MethodDelete, "/api/templates/worker"},
	}

	// Non-admin: every template-mutate route is 403 AND the orchestrator is
	// never reached (the requireAdmin gate returns before proxy runs).
	for _, tc := range mutateCases {
		upstreamHits = 0
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "viewer-sid"})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("non-admin %s: status = %d, want 403", tc.method, rec.Code)
		}
		if upstreamHits != 0 {
			t.Errorf("non-admin %s: orchestrator hit %d times, want 0 (gate must run before proxy)", tc.method, upstreamHits)
		}
	}

	// Unauthenticated: admin routes are API-only → 401 (no login redirect).
	for _, tc := range mutateCases {
		upstreamHits = 0
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauth %s: status = %d, want 401", tc.method, rec.Code)
		}
		if upstreamHits != 0 {
			t.Errorf("unauth %s: orchestrator hit %d times, want 0", tc.method, upstreamHits)
		}
	}

	// Admin: each mutate route passes the gate and is forwarded to the
	// orchestrator (proves the gate is requireAdmin, not a 403-everything).
	for _, tc := range mutateCases {
		upstreamHits = 0
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "admin-sid"})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("admin %s: status = %d, want 200 (forwarded; upstream returns 200)", tc.method, rec.Code)
		}
		if upstreamHits != 1 {
			t.Errorf("admin %s: orchestrator hit %d times, want 1 (forwarded once)", tc.method, upstreamHits)
		}
	}

	// GET /api/templates stays on auth.require: a non-admin is allowed (spawn
	// dropdown unchanged) and forwarded to the orchestrator.
	upstreamHits = 0
	req := httptest.NewRequest(http.MethodGet, "/api/templates", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "viewer-sid"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("non-admin GET /api/templates: status = %d, want 200 (readScope, not admin-gated)", rec.Code)
	}
	if upstreamHits != 1 {
		t.Errorf("non-admin GET /api/templates: orchestrator hit %d times, want 1", upstreamHits)
	}
}

// TestHandleMeReportsIsAdmin asserts /api/me returns isAdmin=true for an admin
// session and isAdmin=false for an ordinary (non-admin) session — the UI gates
// the Agent Types admin view on this flag.
func TestHandleMeReportsIsAdmin(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://identity-server:8080", "dashboard", "secret")
	a.sessions["admin-sid"] = session{username: "alice", isAdmin: true, expiresAt: time.Now().Add(time.Hour)}
	a.sessions["viewer-sid"] = session{username: "viewer", isAdmin: false, expiresAt: time.Now().Add(time.Hour)}

	cases := []struct {
		sid         string
		wantUser    string
		wantIsAdmin bool
	}{
		{"admin-sid", "alice", true},
		{"viewer-sid", "viewer", false},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.sid})
		rec := httptest.NewRecorder()
		a.handleMe(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", tc.sid, rec.Code)
		}
		var got map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", tc.sid, err)
		}
		if got["user"] != tc.wantUser {
			t.Errorf("%s: user = %v, want %q", tc.sid, got["user"], tc.wantUser)
		}
		if isAdmin, _ := got["isAdmin"].(bool); isAdmin != tc.wantIsAdmin {
			t.Errorf("%s: isAdmin = %v, want %v", tc.sid, got["isAdmin"], tc.wantIsAdmin)
		}
	}
}

// TestRequireAdmin asserts the requireAdmin gate rejects a non-admin session
// with 403, an unauthenticated call with 401, and passes an admin session
// through to the wrapped handler. This is the dashboard-BFF enforcement; the
// orchestrator enforces the same role independently (defense in depth).
func TestRequireAdmin(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://identity-server:8080", "dashboard", "secret")
	a.sessions["admin-sid"] = session{username: "alice", isAdmin: true, expiresAt: time.Now().Add(time.Hour)}
	a.sessions["viewer-sid"] = session{username: "viewer", isAdmin: false, expiresAt: time.Now().Add(time.Hour)}

	called := false
	h := a.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	cases := []struct {
		name       string
		sid        string
		wantStatus int
		wantCalled bool
	}{
		{"admin passes", "admin-sid", http.StatusOK, true},
		{"non-admin forbidden", "viewer-sid", http.StatusForbidden, false},
		{"no session unauthorized", "", http.StatusUnauthorized, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = false
			req := httptest.NewRequest("POST", "/api/templates", nil)
			if tc.sid != "" {
				req.AddCookie(&http.Cookie{Name: sessionCookie, Value: tc.sid})
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if called != tc.wantCalled {
				t.Fatalf("handler called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}
