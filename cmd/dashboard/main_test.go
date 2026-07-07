package main

import (
	"context"
	"encoding/base64"
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
	a := NewAuthenticator("http://localhost:8090", "http://keycloak:8080", "dashboard", "secret")
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
	mux := buildMux(a, upstream.URL, "http://keycloak:8080", "http://docs", fstest.MapFS{})

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

// TestKeycloakProxyAllowlist proves the dashboard reverse-proxies ONLY
// Keycloak's browser-facing login surface (/realms/, /resources/) to the
// Keycloak host, forwarding the request path verbatim, and DOES NOT proxy the
// Keycloak admin console (/admin) — a security invariant (the admin console
// must be unreachable through the dashboard origin). It also proves the proxy
// target strips the realm path from IDENTITY_INTERNAL_URL so the /realms/
// prefix isn't doubled.
func TestKeycloakProxyAllowlist(t *testing.T) {
	var gotPath string
	kc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("keycloak"))
	}))
	defer kc.Close()

	a := NewAuthenticator("http://localhost:8090", kc.URL+"/realms/spawnly", "dashboard", "secret")
	// buildMux only needs the auth + a stub upstream for the /api routes.
	mux := buildMux(a, "http://orch.invalid", kc.URL+"/realms/spawnly", "http://docs", fstest.MapFS{})

	// /realms/... is forwarded verbatim (path NOT doubled with /realms/spawnly).
	gotPath = ""
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/realms/spawnly/protocol/openid-connect/auth", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/realms authorize: status = %d, want 200 (proxied)", rec.Code)
	}
	if gotPath != "/realms/spawnly/protocol/openid-connect/auth" {
		t.Errorf("proxied path = %q, want the request path verbatim (no doubled /realms/spawnly)", gotPath)
	}

	// /resources/... (login theme assets) is forwarded.
	gotPath = ""
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/resources/abc/login/keycloak/css/login.css", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/resources: status = %d, want 200 (proxied)", rec.Code)
	}

	// /realms/master/... (a DIFFERENT realm — notably its internet-reachable
	// direct-grant token endpoint) is NOT proxied: the allowlist is scoped to the
	// spawnly realm derived from IDENTITY_INTERNAL_URL. It falls through to the
	// session-gated "/" handler and the upstream Keycloak is NEVER hit.
	gotPath = ""
	rec = httptest.NewRecorder()
	masterReq := httptest.NewRequest("GET", "/realms/master/protocol/openid-connect/token", nil)
	mux.ServeHTTP(rec, masterReq)
	if gotPath != "" {
		t.Fatalf("SECURITY: /realms/master was proxied to Keycloak (path %q) — only the spawnly realm may be reachable via the dashboard origin", gotPath)
	}
	if rec.Code == http.StatusOK {
		t.Errorf("/realms/master: status = %d, want a non-200 (not served)", rec.Code)
	}

	// /admin (the Keycloak admin console + admin REST API) is NOT proxied: it
	// must not reach Keycloak. It falls through to the session-gated "/" handler,
	// which returns 401 for this non-navigation request — critically, the
	// upstream Keycloak is NEVER hit (gotPath stays empty).
	gotPath = ""
	rec = httptest.NewRecorder()
	adminReq := httptest.NewRequest("GET", "/admin/master/console/", nil)
	mux.ServeHTTP(rec, adminReq)
	if gotPath != "" {
		t.Fatalf("SECURITY: /admin was proxied to Keycloak (path %q) — admin console must not be reachable via the dashboard origin", gotPath)
	}
	if rec.Code == http.StatusOK {
		t.Errorf("/admin: status = %d, want a non-200 (not served)", rec.Code)
	}
}

func TestRequireUnauthenticated(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://keycloak:8080", "dashboard", "secret")
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
	a := NewAuthenticator("http://localhost:8090", "http://keycloak:8080", "dashboard", "secret")
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
	a := NewAuthenticator("http://localhost:8090", "http://keycloak:8080", "dashboard", "secret")
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
	mux := buildMux(a, upstream.URL, "http://keycloak:8080", "http://docs", fstest.MapFS{})

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

	// GET /api/templates/spawn (the spawn modal's requiresTenant source) is also
	// on auth.require, NOT requireAdmin: a non-admin must be allowed and forwarded
	// (the spawn UI needs it). A non-admin getting 403 here would break spawning.
	upstreamHits = 0
	req = httptest.NewRequest(http.MethodGet, "/api/templates/spawn", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "viewer-sid"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("non-admin GET /api/templates/spawn: status = %d, want 200 (non-admin spawn list)", rec.Code)
	}
	if upstreamHits != 1 {
		t.Errorf("non-admin GET /api/templates/spawn: orchestrator hit %d times, want 1", upstreamHits)
	}

	// Unauthenticated: spawn list is API-only → 401, orchestrator never reached.
	upstreamHits = 0
	req = httptest.NewRequest(http.MethodGet, "/api/templates/spawn", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unauth GET /api/templates/spawn: status = %d, want 401", rec.Code)
	}
	if upstreamHits != 0 {
		t.Errorf("unauth GET /api/templates/spawn: orchestrator hit %d times, want 0", upstreamHits)
	}
}

// meSession gives a session a live (non-expiring) token source so /api/me can
// fetch /v1/my/orgs through the orchestrator stub.
func meSession(s session) session {
	oauth := &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://refresh.invalid/token"}}
	s.tokenSrc = oauth.TokenSource(context.Background(), &oauth2.Token{AccessToken: "at-1", Expiry: time.Now().Add(time.Hour)})
	s.expiresAt = time.Now().Add(time.Hour)
	return s
}

// TestApiMeComposesOrgs drives the composed GET /api/me: it merges the session
// identity (user/isAdmin/activeOrg) with the orchestrator's /v1/my/orgs
// (orgs/isOrgAdmin), and degrades to orgsUnavailable when the orchestrator errs.
func TestApiMeComposesOrgs(t *testing.T) {
	// Orchestrator stub: /v1/my/orgs echoes the caller's active org from the token —
	// but the test controls the response per session via a switch on the bearer.
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/my/orgs" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"orgs":[{"orgId":"acme","name":"Acme"}],"activeOrg":"acme","isOrgAdmin":true}`))
	}))
	defer orch.Close()

	a := stubAuth(&oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://refresh.invalid/token"}})
	a.sessions["admin-sid"] = meSession(session{username: "alice", isAdmin: true, activeOrg: "acme"})
	a.sessions["viewer-sid"] = meSession(session{username: "viewer", isAdmin: false, activeOrg: "acme"})
	mux := buildMux(a, orch.URL, "http://keycloak:8080", "http://docs", fstest.MapFS{})

	get := func(sid string) map[string]any {
		req := httptest.NewRequest("GET", "/api/me", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200", sid, rec.Code)
		}
		var got map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatalf("%s: decode: %v", sid, err)
		}
		return got
	}

	admin := get("admin-sid")
	if admin["user"] != "alice" || admin["isAdmin"] != true {
		t.Errorf("admin identity wrong: %v", admin)
	}
	if admin["activeOrg"] != "acme" {
		t.Errorf("activeOrg = %v, want acme", admin["activeOrg"])
	}
	if admin["isOrgAdmin"] != true {
		t.Errorf("isOrgAdmin = %v, want true", admin["isOrgAdmin"])
	}
	orgs, _ := admin["orgs"].([]any)
	if len(orgs) != 1 {
		t.Errorf("orgs = %v, want one org", admin["orgs"])
	}
	if _, bad := admin["orgsUnavailable"]; bad {
		t.Error("orgsUnavailable should be absent when the orchestrator answers")
	}

	viewer := get("viewer-sid")
	if viewer["user"] != "viewer" || viewer["isAdmin"] != false {
		t.Errorf("viewer identity wrong: %v", viewer)
	}
}

// TestApiMeDegradesWhenOrchestratorDown proves /api/me still returns the session
// identity with orgsUnavailable=true (not a false no-org screen) when the
// orchestrator's /v1/my/orgs fails, rather than 5xx-ing the whole page.
func TestApiMeDegradesWhenOrchestratorDown(t *testing.T) {
	orch := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer orch.Close()

	a := stubAuth(&oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: "http://refresh.invalid/token"}})
	a.sessions["sid"] = meSession(session{username: "alice", isAdmin: false, activeOrg: ""})
	mux := buildMux(a, orch.URL, "http://keycloak:8080", "http://docs", fstest.MapFS{})

	req := httptest.NewRequest("GET", "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrade, not fail)", rec.Code)
	}
	var got map[string]any
	json.NewDecoder(rec.Body).Decode(&got)
	if got["user"] != "alice" {
		t.Errorf("user = %v, want alice", got["user"])
	}
	if got["orgsUnavailable"] != true {
		t.Errorf("orgsUnavailable = %v, want true", got["orgsUnavailable"])
	}
}

// TestHandleLoginOrgScope asserts /signin?org=<alias> requests the
// organization:<alias> scope on the authorize redirect (per-login single-org
// scope), and that an invalid alias is dropped (org-less) rather than injected.
func TestHandleLoginOrgScope(t *testing.T) {
	a := stubAuth(&oauth2.Config{
		ClientID: "dashboard",
		Endpoint: oauth2.Endpoint{AuthURL: "http://localhost:8090/realms/spawnly/protocol/openid-connect/auth"},
		Scopes:   []string{"openid", "orchestrator:read", "orchestrator:write"},
	})

	scopeOf := func(target string) string {
		req := httptest.NewRequest("GET", target, nil)
		rec := httptest.NewRecorder()
		a.handleLogin(rec, req)
		loc := rec.Header().Get("Location")
		u, err := url.Parse(loc)
		if err != nil {
			t.Fatalf("parse redirect %q: %v", loc, err)
		}
		return u.Query().Get("scope")
	}

	if sc := scopeOf("/signin?org=acme"); !strings.Contains(sc, "organization:acme") {
		t.Errorf("org login scope = %q, want it to contain organization:acme", sc)
	}
	// Invalid alias (uppercase/space) must NOT be spliced into the scope.
	if sc := scopeOf("/signin?org=Acme%20Corp"); strings.Contains(sc, "organization:") {
		t.Errorf("invalid org alias must be dropped, scope = %q", sc)
	}
	// No org param → no organization scope override (base scopes only).
	if sc := scopeOf("/signin"); strings.Contains(sc, "organization:") {
		t.Errorf("org-less login must not request an organization scope, scope = %q", sc)
	}
}

// TestSwitchOrgReAuths asserts /switch-org clears the session and redirects to
// /signin with the new org, so switching orgs is a full re-auth.
func TestSwitchOrgReAuths(t *testing.T) {
	a := stubAuth(&oauth2.Config{})
	a.sessions["sid"] = session{username: "alice", activeOrg: "acme", expiresAt: time.Now().Add(time.Hour)}

	req := httptest.NewRequest("GET", "/switch-org?org=globex", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})
	rec := httptest.NewRecorder()
	a.handleSwitchOrg(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/signin?org=globex" {
		t.Errorf("redirect = %q, want /signin?org=globex", loc)
	}
	if _, ok := a.sessions["sid"]; ok {
		t.Error("switch-org must delete the old session (its old-org token must not survive)")
	}
	// The session cookie must be cleared (MaxAge<0).
	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("session cookie was not cleared on switch-org")
	}
}

// TestParseOrgClaim covers the fail-closed single-org extraction from an access
// token's organization claim (the session's activeOrg source).
func TestParseOrgClaim(t *testing.T) {
	mk := func(orgJSON string) string {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"organization":` + orgJSON + `}`))
		return "h." + payload + ".s"
	}
	cases := []struct {
		name string
		jwt  string
		want string
	}{
		{"single-array", mk(`["acme"]`), "acme"},
		{"multi-array fails closed", mk(`["acme","globex"]`), ""},
		{"empty-array", mk(`[]`), ""},
		{"map of one", mk(`{"acme":{}}`), "acme"},
		{"string", mk(`"acme"`), "acme"},
		{"absent", "h." + base64.RawURLEncoding.EncodeToString([]byte(`{}`)) + ".s", ""},
		{"garbage", "not-a-jwt", ""},
	}
	for _, tc := range cases {
		if got := parseOrgClaim(tc.jwt); got != tc.want {
			t.Errorf("%s: parseOrgClaim = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestRequireAdmin asserts the requireAdmin gate rejects a non-admin session
// with 403, an unauthenticated call with 401, and passes an admin session
// through to the wrapped handler. This is the dashboard-BFF enforcement; the
// orchestrator enforces the same role independently (defense in depth).
func TestRequireAdmin(t *testing.T) {
	a := NewAuthenticator("http://localhost:8090", "http://keycloak:8080", "dashboard", "secret")
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
