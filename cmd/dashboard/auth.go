package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Authenticator turns the dashboard into a confidential OIDC relying party.
//
// Split-horizon note: tokens carry the in-cluster issuer (http://identity-server:8080)
// so the resource servers validate agent and human tokens identically — we must
// NOT change that. The RP therefore does discovery / token exchange / JWKS / id_token
// verification against InternalURL (the issuer). Only the browser-facing pieces use
// Authority (the dashboard origin): the authorize redirect and end_session go through
// the dashboard, which reverse-proxies /connect, /.well-known and /Account to
// identity-server, so the browser never has to reach the cluster-internal name.
type Authenticator struct {
	authority    string // browser-facing dashboard origin, e.g. http://localhost:8090
	internalURL  string // in-cluster identity-server (the token issuer), e.g. http://identity-server:8080
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

type session struct {
	username   string
	rawIDToken string // kept for the logout id_token_hint
	expiresAt  time.Time
}

type pendingLogin struct {
	verifier  string
	createdAt time.Time
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
// the listener is already up and identity-server has had time to come ready.
func (a *Authenticator) ensure(ctx context.Context) error {
	a.once.Do(func() {
		// Discovery, token exchange, JWKS and id_token verification all run
		// against the internal URL, which is the token issuer — so they match
		// with no issuer-rewriting tricks.
		provider, err := oidc.NewProvider(ctx, a.internalURL)
		if err != nil {
			a.initErr = fmt.Errorf("oidc discovery: %w", err)
			return
		}

		// Only the browser-facing authorize redirect uses the dashboard origin
		// (proxied to identity-server). The token endpoint from discovery is the
		// internal URL, which the dashboard reaches directly.
		endpoint := provider.Endpoint()
		endpoint.AuthURL = a.authority + "/connect/authorize"

		a.oauth = &oauth2.Config{
			ClientID:     a.clientID,
			ClientSecret: a.clientSecret,
			Endpoint:     endpoint,
			RedirectURL:  a.authority + "/callback",
			Scopes:       []string{oidc.ScopeOpenID, "profile"},
		}

		a.verifier = provider.Verifier(&oidc.Config{ClientID: a.clientID})
	})
	return a.initErr
}

// handleLogin starts the authorization-code+PKCE flow.
func (a *Authenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := a.ensure(r.Context()); err != nil {
		http.Error(w, "auth unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}

	state := randToken()
	verifier := oauth2.GenerateVerifier()

	a.mu.Lock()
	a.pending[state] = pendingLogin{verifier: verifier, createdAt: time.Now()}
	a.gcPendingLocked()
	a.mu.Unlock()

	// Bind the state to the browser to defend the callback against CSRF.
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/",
		HttpOnly: true, Secure: forwardedHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: 600,
	})

	authURL := a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, authURL, http.StatusFound)
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
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "cannot read id_token claims", http.StatusBadGateway)
		return
	}
	username := firstNonEmpty(claims.PreferredUsername, claims.Sub)

	sid := randToken()
	a.mu.Lock()
	a.sessions[sid] = session{username: username, rawIDToken: rawID, expiresAt: time.Now().Add(8 * time.Hour)}
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

	end := a.authority + "/connect/endsession?post_logout_redirect_uri=" +
		url.QueryEscape(a.authority+"/")
	if hint != "" {
		end += "&id_token_hint=" + url.QueryEscape(hint)
	}
	http.Redirect(w, r, end, http.StatusFound)
}

// handleMe reports the logged-in user (the UI shows it and confirms the session).
func (a *Authenticator) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := a.user(r) // require() guarantees a session before this runs
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"user": user})
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
