package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// satAuth returns a stub Authenticator configured for the org-subdomains feature
// against the given apex, with a stub oauth config whose AuthURL is the apex
// authorize endpoint (so tests can assert the authorize host stays the apex).
func satAuth(apex string) *Authenticator {
	a := stubAuth(&oauth2.Config{
		ClientID: "dashboard",
		Endpoint: oauth2.Endpoint{AuthURL: "http://localhost:8090/realms/spawnly/protocol/openid-connect/auth"},
		Scopes:   []string{"openid", "orchestrator:read", "orchestrator:write"},
	})
	a.hosts = hostConfig{apexDomain: apex}
	return a
}

// TestSatelliteAuthorizeURL proves a login STARTED on a satellite assembles the
// authorize redirect with: the authorize host = the apex (single auth origin), the
// redirect_uri = the satellite's own /callback, the `organization` auth param, and
// the organization:<slug> scope — with the org taken from the HOST (not a ?org=).
func TestSatelliteAuthorizeURL(t *testing.T) {
	a := satAuth("localtest.me")

	req := httptest.NewRequest("GET", "/signin?org=globex", nil) // ?org= must be overridden by the host
	req.Host = "acme.localtest.me"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	a.handleLogin(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	// Authorize host is the apex auth origin, never the satellite.
	if loc.Host != "localhost:8090" {
		t.Errorf("authorize host = %q, want the apex auth origin localhost:8090 (satellites must not host authorize)", loc.Host)
	}
	q := loc.Query()
	if ru := q.Get("redirect_uri"); ru != "https://acme.localtest.me/callback" {
		t.Errorf("redirect_uri = %q, want https://acme.localtest.me/callback", ru)
	}
	if org := q.Get("organization"); org != "acme" {
		t.Errorf("organization param = %q, want acme (host org, overriding ?org=globex)", org)
	}
	if sc := q.Get("scope"); !strings.Contains(sc, "organization:acme") {
		t.Errorf("scope = %q, want it to contain organization:acme", sc)
	}
	if strings.Contains(q.Get("scope"), "organization:globex") {
		t.Errorf("scope = %q must not carry the ?org=globex the host overrode", q.Get("scope"))
	}
}

// TestApexAuthorizeURLUnchanged proves a login on the APEX (or in single-origin
// mode) sends NO redirect_uri override (the config's apex /callback is used) — the
// existing single-origin behavior. The ?org= still drives the org there.
func TestApexAuthorizeURLUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name string
		apex string
		host string
	}{
		{"apex host", "localtest.me", "localtest.me"},
		{"feature off", "", "localhost:8090"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := satAuth(tc.apex)
			req := httptest.NewRequest("GET", "/signin?org=acme", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			a.handleLogin(rec, req)

			loc, _ := url.Parse(rec.Header().Get("Location"))
			q := loc.Query()
			if q.Has("redirect_uri") {
				t.Errorf("apex/off login must not override redirect_uri, got %q", q.Get("redirect_uri"))
			}
			// The `organization` authorize param is satellite-only: an apex /
			// single-origin authorize request must be byte-identical to the
			// pre-feature flow (which never sent it), so the just-settled apex
			// silent org-switch is untouched.
			if q.Has("organization") {
				t.Errorf("apex/off login must not send the organization param, got %q", q.Get("organization"))
			}
			// ?org= still drives the org on the apex.
			if !strings.Contains(q.Get("scope"), "organization:acme") {
				t.Errorf("apex ?org=acme scope = %q, want organization:acme", q.Get("scope"))
			}
		})
	}
}

// TestCallbackReplaysSatelliteRedirectURI proves the code exchange sends the SAME
// redirect_uri the authorize request used (RFC 6749 §4.1.3): the recorded
// satellite /callback, or none when the login was on the apex.
func TestCallbackReplaysSatelliteRedirectURI(t *testing.T) {
	exchange := func(recorded string) string {
		var gotRedirect string
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			gotRedirect = r.Form.Get("redirect_uri")
			w.Header().Set("Content-Type", "application/json")
			// A syntactically valid token WITHOUT an id_token: Exchange succeeds (so
			// the redirect_uri was already sent + captured), then handleCallback 502s
			// at the id_token check — no verifier needed for this assertion.
			_, _ = w.Write([]byte(`{"access_token":"at","token_type":"Bearer"}`))
		}))
		defer ts.Close()

		a := stubAuth(&oauth2.Config{
			ClientID:     "dashboard",
			ClientSecret: "secret",
			Endpoint:     oauth2.Endpoint{TokenURL: ts.URL + "/token"},
		})
		a.pending["st"] = pendingLogin{verifier: "v", createdAt: time.Now(), org: "acme", redirectURI: recorded}
		req := httptest.NewRequest("GET", "/callback?state=st&code=xyz", nil)
		req.AddCookie(&http.Cookie{Name: stateCookie, Value: "st"})
		a.handleCallback(httptest.NewRecorder(), req)
		return gotRedirect
	}

	if got := exchange("https://acme.localtest.me/callback"); got != "https://acme.localtest.me/callback" {
		t.Errorf("satellite exchange redirect_uri = %q, want the recorded satellite /callback", got)
	}
	if got := exchange(""); got != "" {
		t.Errorf("apex exchange redirect_uri = %q, want empty (config default used)", got)
	}
}

// TestSelfHealDecisionMatrix pins require()'s host↔claim self-heal decisions:
// match → serve; page-nav mismatch → /switch-org?org=<hostOrg> (guarded); a
// second-pass mismatch (guard set) → fall through to the denied screen; an /api/*
// request → never self-heal (host is not authorization).
func TestSelfHealDecisionMatrix(t *testing.T) {
	newAuth := func(activeOrg string) *Authenticator {
		a := satAuth("localtest.me")
		a.sessions["sid"] = session{username: "alice", activeOrg: activeOrg, expiresAt: time.Now().Add(time.Hour)}
		return a
	}
	// pageReq builds an authenticated page navigation on the given host.
	pageReq := func(path, host, guard string) *http.Request {
		req := httptest.NewRequest("GET", path, nil)
		req.Host = host
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})
		if guard != "" {
			req.AddCookie(&http.Cookie{Name: orgHealCookie, Value: guard})
		}
		return req
	}
	guardCookie := func(rec *httptest.ResponseRecorder) *http.Cookie {
		for _, c := range rec.Result().Cookies() {
			if c.Name == orgHealCookie {
				return c
			}
		}
		return nil
	}

	// Match: activeOrg == hostOrg → no self-heal, and any stale guard is cleared.
	t.Run("match serves", func(t *testing.T) {
		a := newAuth("acme")
		rec := httptest.NewRecorder()
		if a.selfHeal(rec, pageReq("/", "acme.localtest.me", "acme")) {
			t.Fatal("match must not self-heal")
		}
		if c := guardCookie(rec); c == nil || c.MaxAge >= 0 {
			t.Error("a match should clear the heal guard cookie")
		}
	})

	// Mismatch, first pass → redirect to /switch-org?org=acme with the guard set.
	t.Run("mismatch redirects", func(t *testing.T) {
		a := newAuth("globex")
		rec := httptest.NewRecorder()
		if !a.selfHeal(rec, pageReq("/", "acme.localtest.me", "")) {
			t.Fatal("mismatch must self-heal (redirect)")
		}
		if loc := rec.Header().Get("Location"); loc != "/switch-org?org=acme" {
			t.Errorf("redirect = %q, want /switch-org?org=acme", loc)
		}
		if c := guardCookie(rec); c == nil || c.Value != "acme" || c.MaxAge <= 0 {
			t.Errorf("mismatch must set the heal guard to the host org, got %+v", c)
		}
	})

	// Org-less session (non-member after the silent switch) still mismatches → the
	// FIRST pass would redirect; the guard makes the SECOND pass fall through.
	t.Run("second-pass mismatch falls through to denied", func(t *testing.T) {
		a := newAuth("") // org-less token: non-member of acme
		rec := httptest.NewRecorder()
		if a.selfHeal(rec, pageReq("/", "acme.localtest.me", "acme")) {
			t.Fatal("a guarded persistent mismatch must NOT redirect again (would loop)")
		}
		if c := guardCookie(rec); c == nil || c.MaxAge >= 0 {
			t.Error("the guard should be cleared when falling through to the denied screen")
		}
	})

	// /api/* never self-heals even on a mismatch — the orchestrator enforces from
	// the token; host is not authorization.
	t.Run("api never self-heals", func(t *testing.T) {
		a := newAuth("globex")
		rec := httptest.NewRecorder()
		if a.selfHeal(rec, pageReq("/api/agents", "acme.localtest.me", "")) {
			t.Fatal("/api/* must never self-heal")
		}
	})

	// A subresource (non-document) navigation never self-heals.
	t.Run("subresource never self-heals", func(t *testing.T) {
		a := newAuth("globex")
		req := httptest.NewRequest("GET", "/app.js", nil)
		req.Host = "acme.localtest.me"
		req.Header.Set("Sec-Fetch-Dest", "script")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "sid"})
		if a.selfHeal(httptest.NewRecorder(), req) {
			t.Fatal("a subresource request must never self-heal")
		}
	})

	// On the apex, a session holding any org never self-heals (the apex hosts all).
	t.Run("apex never self-heals", func(t *testing.T) {
		a := newAuth("globex")
		if a.selfHeal(httptest.NewRecorder(), pageReq("/", "localtest.me", "")) {
			t.Fatal("the apex must never self-heal")
		}
	})
}
