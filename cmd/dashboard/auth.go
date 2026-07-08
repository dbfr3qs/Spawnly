package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// errNoSession is returned by orchestratorToken when the request carries no
// (valid) dashboard session — callers turn it into a fail-closed error rather
// than calling the orchestrator unauthenticated.
var errNoSession = errors.New("no dashboard session")

// Authenticator turns the dashboard into a confidential OIDC relying party.
//
// Split-horizon note: tokens carry the in-cluster issuer (the Keycloak realm,
// http://keycloak:8080/realms/spawnly) so the resource servers validate agent
// and human tokens identically — we must NOT change that. The RP therefore does
// discovery / token exchange / JWKS / id_token verification against InternalURL
// (the realm issuer). Only the browser-facing pieces use Authority (the
// dashboard origin): the authorize redirect and end_session go through the
// dashboard, which reverse-proxies Keycloak's browser-facing paths (/realms,
// /resources) so the browser never has to reach the cluster-internal name.
type Authenticator struct {
	authority    string // browser-facing dashboard origin, e.g. http://localhost:8090
	internalURL  string // in-cluster Keycloak realm issuer, e.g. http://keycloak:8080/realms/spawnly
	clientID     string
	clientSecret string

	once     sync.Once
	initErr  error
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier

	mu       sync.Mutex
	sessions map[string]session      // session cookie value -> session
	pending  map[string]pendingLogin // OIDC state -> PKCE verifier
}

// validOrgAlias is the org alias/slug charset (a DNS label) — the SAME shape the
// registry enforces (cmd/registry/orgs.go validOrgID). Applied before an org
// alias is spliced into an OAuth scope string (organization:<alias>) or a redirect
// query, so a crafted value can never inject whitespace/newlines into the scope or
// smuggle extra request params. An invalid alias is treated as org-less (dropped).
var validOrgAlias = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type session struct {
	username   string
	rawIDToken string // kept for the logout id_token_hint
	isAdmin    bool
	// activeOrg is the single organization this session's token names (the
	// `organization` claim), parsed from the access token at callback. "" for an
	// org-less (global) session — a realm admin, or a fresh login before an org was
	// picked. Single-org by construction: the dashboard requests organization:<alias>
	// so the token names exactly one org (see handleLogin). The org picker / no-org
	// screen (static UI) and /switch-org drive changes to it.
	activeOrg string
	// tokenSrc yields the delegated OAuth access token the dashboard presents to
	// the orchestrator (audienced for "orchestrator", sub=userId, scope
	// orchestrator:read/write). It is a ReuseTokenSource: it caches the current
	// access token and refreshes it (once, under its own mutex) via the refresh
	// token when it expires — so the dashboard's concurrent /api/* calls don't
	// race to redeem a rotating refresh token.
	tokenSrc  oauth2.TokenSource
	expiresAt time.Time
}

type pendingLogin struct {
	verifier  string
	createdAt time.Time
	// org is the organization this flow requests (organization:<alias> scope), or
	// "" for an org-less/global login. Kept so a silent-attempt fallback can
	// re-drive the SAME org interactively.
	org string
	// silent marks an org-switch that tried prompt=none (no user interaction,
	// reusing the existing Keycloak SSO session). When Keycloak answers that it
	// can't complete without interaction, the callback falls back to an
	// interactive login for `org` instead of erroring.
	silent bool
}

const (
	sessionCookie = "dash_session"
	stateCookie   = "dash_oidc_state"
)

func NewAuthenticator(authority, internalURL, clientID, clientSecret string) *Authenticator {
	return &Authenticator{
		authority:    strings.TrimRight(authority, "/"),
		internalURL:  strings.TrimRight(internalURL, "/"),
		clientID:     clientID,
		clientSecret: clientSecret,
		sessions:     map[string]session{},
		pending:      map[string]pendingLogin{},
	}
}

// ensure lazily performs OIDC discovery. Done on first use (not at startup) so
// the listener is already up and Keycloak has had time to come ready.
func (a *Authenticator) ensure(ctx context.Context) error {
	a.once.Do(func() {
		// Split-horizon under Keycloak's fixed-frontend-hostname model: the
		// token issuer is the dashboard ORIGIN (KC_HOSTNAME), but the dashboard
		// reaches Keycloak in-cluster at internalURL (keycloak:8080). Fetch
		// discovery from internalURL yet EXPECT the issuer to be the dashboard
		// origin, so wrap the ctx to override go-oidc's issuer==discovery-URL
		// check. Backchannel endpoints (token/JWKS) still come back as
		// keycloak:8080 (KC_HOSTNAME_BACKCHANNEL_DYNAMIC), which the dashboard
		// reaches directly; only the browser-facing authorize URL uses the
		// origin (overridden below). The id_token's iss is the origin, so the
		// verifier (built from this provider) checks against the origin too.
		issuerURL := a.authority + a.realmPath()
		provider, err := oidc.NewProvider(oidc.InsecureIssuerURLContext(ctx, issuerURL), a.internalURL)
		if err != nil {
			a.initErr = fmt.Errorf("oidc discovery: %w", err)
			return
		}

		// Only the browser-facing authorize redirect uses the dashboard origin
		// (proxied to Keycloak). The token endpoint from discovery is the
		// internal realm URL, which the dashboard reaches directly. Keycloak's
		// browser-facing authorize path is
		// <origin>/realms/<realm>/protocol/openid-connect/auth; the realm path
		// (/realms/<realm>) is taken from the issuer so it isn't duplicated.
		endpoint := provider.Endpoint()
		endpoint.AuthURL = a.authority + a.realmPath() + "/protocol/openid-connect/auth"

		a.oauth = &oauth2.Config{
			ClientID:     a.clientID,
			ClientSecret: a.clientSecret,
			Endpoint:     endpoint,
			RedirectURL:  a.authority + "/callback",
			// openid/profile drive the human's id_token; spawnly-roles carries the
			// user's flat `role` claim (admin gating) into the id_token;
			// orchestrator:read/write are the delegated authority the access token
			// carries to the orchestrator; offline_access mints the refresh token
			// that keeps it fresh for the session's lifetime. NOTE: the scope is
			// "spawnly-roles", not "roles" — Keycloak's built-in `roles` scope
			// emits realm_access.roles; our flat `role` claim lives in a distinct
			// scope (see deploy/keycloak/scopes.tf).
			Scopes: []string{oidc.ScopeOpenID, "profile", "spawnly-roles", "orchestrator:read", "orchestrator:write", "offline_access"},
		}

		a.verifier = provider.Verifier(&oidc.Config{ClientID: a.clientID})
	})
	return a.initErr
}

// handleLogin starts the (interactive) authorization-code+PKCE flow, reading the
// optional ?org=<alias> to request a single-org token.
func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := a.ensure(r.Context()); err != nil {
		http.Error(w, "auth unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	org := ""
	if o := r.URL.Query().Get("org"); o != "" && validOrgAlias.MatchString(o) {
		org = o
	}
	a.startAuth(w, r, org, false, "")
}

// startAuth begins the auth-code+PKCE flow for org (may be "" for org-less).
// When silent, it adds prompt=none (+ login_hint) so an EXISTING Keycloak SSO
// session yields a fresh single-org token with NO user interaction; if Keycloak
// answers that it can't complete silently, handleCallback falls back to an
// interactive login for the same org. hint is the login_hint (the current
// username) — only meaningful when silent.
func (a *Authenticator) startAuth(w http.ResponseWriter, r *http.Request, org string, silent bool, hint string) {
	state := randToken()
	verifier := oauth2.GenerateVerifier()

	a.mu.Lock()
	a.pending[state] = pendingLogin{verifier: verifier, createdAt: time.Now(), org: org, silent: silent}
	a.gcPendingLocked()
	a.mu.Unlock()

	// Bind the state to the browser to defend the callback against CSRF.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/",
		HttpOnly: true, Secure: forwardedHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})

	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(verifier)}
	// Per-login single-org scope: when the login names an org, request
	// organization:<alias> so Keycloak stamps that single active org into the
	// token. The scope set is thus per-login (not the static config list): base
	// scopes + this one org. Keycloak only issues the claim if the user is actually
	// a member — a non-member request yields an org-less token (verified against a
	// live realm), so this cannot be used to claim an org the user does not belong
	// to. Overriding the authorize "scope" param (SetAuthURLParam runs after
	// AuthCodeURL sets it from Scopes) is what makes the scope per-login; the code
	// exchange + refresh preserve the granted scopes, so the org rides the whole
	// session without more plumbing.
	if org != "" {
		scopes := append(append([]string{}, a.oauth.Scopes...), "organization:"+org)
		opts = append(opts, oauth2.SetAuthURLParam("scope", strings.Join(scopes, " ")))
	}
	// Silent org switch: reuse the live Keycloak SSO session for the
	// already-signed-in user (prompt=none = no UI). login_hint names that user so
	// Keycloak's identity-first (Organizations) flow can resolve them without the
	// username screen. If there is no usable session, Keycloak redirects back with
	// ?error=login_required (or interaction_required/consent_required), which the
	// callback turns into an interactive retry — so the worst case is today's
	// prompted login, never a dead end.
	if silent {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", "none"))
		if hint != "" {
			opts = append(opts, oauth2.SetAuthURLParam("login_hint", hint))
		}
	}

	authURL := a.oauth.AuthCodeURL(state, opts...)
	http.Redirect(w, r, authURL, http.StatusFound)
}

// handleSwitchOrg switches the active org, minting a NEW single-org token for the
// same user (the settled re-auth-per-org decision keeps tokens single-org). It
// clears the current session — the old-org token must not survive the switch —
// then attempts a SILENT re-auth (prompt=none + login_hint) so the switch needs
// no username/password: the Keycloak SSO session for the signed-in user is left
// intact (this never calls end_session), so it can back the silent flow. If
// Keycloak can't complete silently, the callback falls back to an interactive
// login for the same org. An empty/omitted org switches to the org-less (global)
// session (a realm admin picking "global"). The org alias is validated before it
// reaches a scope string.
//
// The silent path engages on HTTPS: Keycloak's SSO cookies are Secure+SameSite=None
// (SameSite=None mandates Secure), so a browser only sends them over https. Over
// plain http (a bare kind cluster) they are held but never transmitted, so the
// silent attempt returns login_required and this gracefully falls back to the
// interactive login — i.e. no regression anywhere, password-less where TLS is
// present (the real deploy behind the ALB).
func (a *Authenticator) handleSwitchOrg(w http.ResponseWriter, r *http.Request) {
	if err := a.ensure(r.Context()); err != nil {
		http.Error(w, "auth unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Capture the signed-in user BEFORE dropping the session, to hint the silent
	// re-auth toward that same user.
	var hint string
	if sid, err := r.Cookie(sessionCookie); err == nil && sid.Value != "" {
		a.mu.Lock()
		if s, ok := a.sessions[sid.Value]; ok {
			hint = s.username
		}
		delete(a.sessions, sid.Value)
		a.mu.Unlock()
	}
	clearCookie(w, sessionCookie, forwardedHTTPS(r))

	org := ""
	if o := r.URL.Query().Get("org"); o != "" && validOrgAlias.MatchString(o) {
		org = o
	}
	a.startAuth(w, r, org, true, hint)
}

// parseOrgClaim extracts the single active org from a raw JWT's `organization`
// claim, for the dashboard's OWN session bookkeeping (display + picker state). It
// decodes the payload WITHOUT verifying the signature — the token is one the
// dashboard just obtained from its own IdP over the back channel and only uses for
// display; the orchestrator independently validates + re-derives the org for every
// authorization decision, so this is never a trust boundary. Fail-closed and shape-
// tolerant, mirroring tokenvalidator.parseOrgID: only a claim naming EXACTLY one
// org yields it; zero, multiple, or an unparsable claim yields "" (org-less).
func parseOrgClaim(rawJWT string) string {
	parts := strings.Split(rawJWT, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var claims struct {
		Organization any `json:"organization"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	switch t := claims.Organization.(type) {
	case []any:
		if len(t) == 1 {
			if s, ok := t[0].(string); ok {
				return s
			}
		}
	case map[string]any:
		if len(t) == 1 {
			for k := range t {
				return k
			}
		}
	case string:
		return t
	}
	return ""
}

// handleCallback completes the flow: validate state, exchange the code, verify
// the id_token, and establish a session.
func (a *Authenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	if err := a.ensure(r.Context()); err != nil {
		http.Error(w, "auth unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}

	state := r.URL.Query().Get("state")
	stateCk, err := r.Cookie(stateCookie)
	if err != nil || stateCk.Value == "" || stateCk.Value != state {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	clearCookie(w, stateCookie, forwardedHTTPS(r))

	a.mu.Lock()
	pl, ok := a.pending[state]
	delete(a.pending, state)
	a.mu.Unlock()
	if !ok {
		http.Error(w, "unknown state", http.StatusBadRequest)
		return
	}

	// Keycloak signals a flow it couldn't complete by redirecting back with
	// ?error= instead of ?code=. For a SILENT org switch (prompt=none) the
	// expected error is login_required / interaction_required / consent_required
	// — there was no reusable session, so fall back to an interactive login for
	// the SAME org (no dead end, no worse than a normal prompted login). For a
	// non-silent flow an error is a real failure surfaced to the user.
	if authErr := r.URL.Query().Get("error"); authErr != "" {
		if pl.silent {
			target := "/signin"
			if pl.org != "" {
				target += "?org=" + url.QueryEscape(pl.org)
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		http.Error(w, "login failed: "+authErr, http.StatusBadRequest)
		return
	}

	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	tok, err := a.oauth.Exchange(r.Context(), code, oauth2.VerifierOption(pl.verifier))
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	rawID, ok := tok.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token in response", http.StatusBadGateway)
		return
	}
	idToken, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil {
		http.Error(w, "id_token verification failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	var claims struct {
		Sub               string `json:"sub"`
		PreferredUsername string `json:"preferred_username"`
		Name              string `json:"name"`
		Role              any    `json:"role"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "cannot read id_token claims", http.StatusBadGateway)
		return
	}
	username := firstNonEmpty(claims.PreferredUsername, claims.Sub)
	isAdmin := roleIsAdmin(claims.Role)

	sid := randToken()
	// Build the reusable, refresh-capable token source once for the session.
	// context.Background() (not the request ctx) so refreshes aren't bound to a
	// single request's lifetime; refresh runs against the discovered (internal)
	// token endpoint, preserving split-horizon.
	tokenSrc := a.oauth.TokenSource(context.Background(), tok)
	// Record the session's active org from the ACCESS token's organization claim
	// (the token the dashboard presents to the orchestrator, and the one that
	// carries the org). "" for an org-less login; a single alias when an org scope
	// was requested and the user is a member.
	activeOrg := parseOrgClaim(tok.AccessToken)
	a.mu.Lock()
	a.sessions[sid] = session{username: username, isAdmin: isAdmin, activeOrg: activeOrg, rawIDToken: rawID, tokenSrc: tokenSrc, expiresAt: time.Now().Add(8 * time.Hour)}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: sid, Path: "/",
		HttpOnly: true, Secure: forwardedHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout clears the local session, then sends the browser to the IdP's
// end_session endpoint (via the proxy) so the IdP cookie is killed too.
func (a *Authenticator) handleLogout(w http.ResponseWriter, r *http.Request) {
	var hint string
	if sid, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		if s, ok := a.sessions[sid.Value]; ok {
			hint = s.rawIDToken
			delete(a.sessions, sid.Value)
		}
		a.mu.Unlock()
	}
	clearCookie(w, sessionCookie, forwardedHTTPS(r))

	// Keycloak's browser-facing end-session (RP-initiated logout) endpoint,
	// proxied through the dashboard origin so the browser stays single-origin.
	end := a.authority + a.realmPath() + "/protocol/openid-connect/logout?post_logout_redirect_uri=" +
		url.QueryEscape(a.authority+"/")
	if hint != "" {
		end += "&id_token_hint=" + url.QueryEscape(hint)
	}
	http.Redirect(w, r, end, http.StatusFound)
}

// realmPath returns the issuer's path portion (e.g. "/realms/spawnly") so the
// browser-facing authorize/logout URLs can be built on the dashboard origin
// without hardcoding the realm name. Keycloak's issuer is
// <url>/realms/<realm>, so the path is exactly the realm segment the proxy
// (main.go) forwards to Keycloak. Falls back to "" if the issuer can't be
// parsed (a misconfigured internalURL surfaces later at discovery anyway).
func (a *Authenticator) realmPath() string {
	u, err := url.Parse(a.internalURL)
	if err != nil {
		return ""
	}
	return strings.TrimRight(u.Path, "/")
}

// require gates a handler behind a valid session. Unauthenticated API calls get
// 401; only genuine top-level page navigations are redirected to /signin (which
// starts the OIDC flow and lands the browser on the /login form).
func (a *Authenticator) require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.user(r); ok {
			next.ServeHTTP(w, r)
			return
		}
		// Start the OIDC login only for real page navigations. API calls get 401;
		// so does every other subresource request (notably the browser's eager
		// /favicon.ico) — redirecting those to /signin would mint a second login
		// state and overwrite the cookie of the in-flight navigation, surfacing
		// as an "invalid state" error on the callback.
		if strings.HasPrefix(r.URL.Path, "/api/") || !isPageNavigation(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/signin", http.StatusFound)
	})
}

// isAdmin reports whether the request's session belongs to an admin user.
func (a *Authenticator) isAdmin(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	a.mu.Lock()
	s, ok := a.sessions[c.Value]
	if ok && time.Now().After(s.expiresAt) {
		delete(a.sessions, c.Value)
		ok = false
	}
	a.mu.Unlock()
	return ok && s.isAdmin
}

// requireAdmin gates a handler behind an admin session (role=admin) in addition
// to a valid session. An unauthenticated caller gets 401 (admin routes are
// API-only, so no login redirect); an authenticated non-admin gets 403. This is
// the dashboard-side enforcement of the admin role — the orchestrator enforces
// the same role on its own admin routes (defense in depth). UI hiding is
// cosmetic and must not be the only gate.
func (a *Authenticator) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := a.user(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !a.isAdmin(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// roleIsAdmin reports whether a raw "role" claim value includes "admin". The
// claim is a JSON string for a single role and a JSON array for multiple; absent
// or empty ⇒ false (fail-closed: a user with no role claim is not an admin).
// Mirrors tokenvalidator.parseStringOrArray's three shapes (string, []string,
// []any) so the BFF and orchestrator enforce the same claim identically and
// cannot drift.
func roleIsAdmin(v any) bool {
	switch t := v.(type) {
	case string:
		return t == "admin"
	case []string:
		for _, s := range t {
			if s == "admin" {
				return true
			}
		}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s == "admin" {
				return true
			}
		}
	}
	return false
}

// isPageNavigation reports whether the request is a top-level browser navigation
// (a document load) rather than a subresource fetch. Prefers the Fetch Metadata
// header (Sec-Fetch-Dest: document) modern browsers always send on navigations;
// falls back to an explicit text/html Accept for older clients.
func isPageNavigation(r *http.Request) bool {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// orchestratorToken returns a valid orchestrator access token for the request's
// session, refreshing it transparently via the OAuth refresh token when it is
// near/after expiry. The rotated token is cached inside the session's shared
// ReuseTokenSource (not rewritten into the session map), so it survives across
// requests. It returns errNoSession when there is no (live) session — callers
// map that to a 401 — and a wrapped error when the session has no token source
// or a refresh fails (callers fail closed with a 5xx); never an empty/stale token.
//
// Refresh runs against the discovered token endpoint, which is the internal
// Keycloak URL (split-horizon: see ensure), so the back-channel never depends
// on the browser-facing origin.
func (a *Authenticator) orchestratorToken(ctx context.Context, r *http.Request) (string, error) {
	if err := a.ensure(ctx); err != nil {
		return "", err
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", errNoSession
	}
	a.mu.Lock()
	s, ok := a.sessions[c.Value]
	if ok && time.Now().After(s.expiresAt) {
		delete(a.sessions, c.Value)
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return "", errNoSession
	}
	if s.tokenSrc == nil {
		return "", errors.New("session has no orchestrator token")
	}

	// The session's ReuseTokenSource returns the cached access token, refreshing
	// (once, under its own mutex) when expired — safe under concurrent calls.
	tok, err := s.tokenSrc.Token()
	if err != nil {
		return "", fmt.Errorf("refresh orchestrator token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("empty orchestrator access token")
	}
	return tok.AccessToken, nil
}

// user returns the logged-in username for the request, if any.
func (a *Authenticator) user(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", false
	}
	a.mu.Lock()
	s, ok := a.sessions[c.Value]
	if ok && time.Now().After(s.expiresAt) {
		delete(a.sessions, c.Value)
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return "", false
	}
	return s.username, true
}

// activeOrg returns the session's active organization alias (the token's
// organization claim), or "" for an org-less session or no live session.
func (a *Authenticator) activeOrg(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	a.mu.Lock()
	s, ok := a.sessions[c.Value]
	if ok && time.Now().After(s.expiresAt) {
		delete(a.sessions, c.Value)
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return ""
	}
	return s.activeOrg
}

func (a *Authenticator) gcPendingLocked() {
	cutoff := time.Now().Add(-10 * time.Minute)
	for k, v := range a.pending {
		if v.createdAt.Before(cutoff) {
			delete(a.pending, k)
		}
	}
}

func clearCookie(w http.ResponseWriter, name string, secure bool) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: secure, MaxAge: -1})
}

// forwardedHTTPS reports whether the original browser request arrived over HTTPS,
// per the load balancer's X-Forwarded-Proto header. The dashboard listens on plain
// HTTP behind the TLS-terminating EKS ALB, so it can't read the scheme directly;
// this is how cookies are marked Secure in production while the local HTTP flow
// (kind / port-forward, no such header) keeps working.
func forwardedHTTPS(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func randToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
