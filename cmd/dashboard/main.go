package main

import (
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

//go:embed static
var staticFiles embed.FS

func main() {
	orchestratorURL := os.Getenv("ORCHESTRATOR_URL")
	if orchestratorURL == "" {
		orchestratorURL = "http://orchestrator:8080"
	}

	// OIDC config. Authority is the browser-facing origin (== IdentityServer
	// IssuerUri); identityInternalURL is the in-cluster back-channel target.
	authority := getenv("OIDC_AUTHORITY", "http://localhost:8090")
	identityInternalURL := getenv("IDENTITY_INTERNAL_URL", "http://identity-server:8080")
	clientID := getenv("OIDC_CLIENT_ID", "dashboard")
	clientSecret := getenv("OIDC_CLIENT_SECRET", "dashboard-secret")
	auth := NewAuthenticator(authority, identityInternalURL, clientID, clientSecret)

	// Browser-facing docs link target. Local dev runs the docs site on :4321;
	// the AWS/prod deploy.sh sets this to the public docs origin.
	docsURL := getenv("DOCS_URL", "http://localhost:4321")

	staticFS, _ := fs.Sub(staticFiles, "static")
	mux := buildMux(auth, orchestratorURL, identityInternalURL, docsURL, staticFS)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("dashboard listening on :%s (orchestrator: %s, oidc authority: %s)", port, orchestratorURL, authority)
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // Slowloris defense; no risk to legit responses.
		ReadTimeout:       30 * time.Second, // request bodies are all small JSON.
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: the dashboard PROXIES the orchestrator's
		// /v1/agents/{id}/logs and /v1/agents/{id}/message, which are
		// legitimately long-lived; a write deadline would truncate them.
	}
	log.Fatal(srv.ListenAndServe())
}

// buildMux wires the dashboard's HTTP routes: the public OIDC relying-party /
// reverse-proxy endpoints, the session-gated static UI, and the /api/* handlers
// that proxy to the orchestrator carrying the session's delegated access token.
func buildMux(auth *Authenticator, orchestratorURL, identityInternalURL, docsURL string, staticFS fs.FS) *http.ServeMux {
	mux := http.NewServeMux()

	// Reverse-proxy the browser-facing OIDC endpoints to identity-server so the
	// browser only ever talks to the dashboard origin (single issuer). These are
	// public — they must not be gated by require().
	idTarget, err := url.Parse(identityInternalURL)
	if err != nil {
		log.Fatalf("invalid IDENTITY_INTERNAL_URL: %v", err)
	}
	idProxy := httputil.NewSingleHostReverseProxy(idTarget)
	mux.Handle("/connect/", idProxy)
	mux.Handle("/.well-known/", idProxy)
	mux.Handle("/Account/", idProxy)
	// The IdentityServer login page is served at /login (its Razor @page route);
	// proxy it (GET render + POST submit) so the browser sees a clean /login URL
	// on the dashboard origin. The dashboard's own login *initiator* lives at
	// /signin (below) — it can't also own /login.
	mux.Handle("/login", idProxy)
	// Pending spawn-consent is now served by the registry-native broker via the
	// /api/consent-requests routes below (Phase 5b); the old IdentityServer
	// /ciba/ browser approval path has been removed.

	// Relying-party routes (public). /signin starts the OIDC code+PKCE flow
	// (require() redirects here); the browser then rests on the /login form above.
	mux.HandleFunc("GET /signin", auth.handleLogin)
	mux.HandleFunc("GET /callback", auth.handleCallback)
	mux.HandleFunc("POST /logout", auth.handleLogout)

	// Static UI behind a session (browser GETs redirect to /signin when absent).
	// Method-less so it doesn't conflict with the method-less OIDC proxy
	// prefixes (Go 1.22 mux rejects a "GET /" catch-all alongside "/connect/").
	mux.Handle("/", auth.require(http.FileServer(http.FS(staticFS))))

	// Proxy handlers — forward to orchestrator, copy status+headers+body verbatim.
	// Every orchestrator call carries the session's delegated access token as
	// `Authorization: Bearer` (the orchestrator derives userId + scopes from it).
	// We send CLEAN headers — only Content-Type and the bearer — so the browser's
	// own cookies/Authorization never leak upstream. A missing/unrefreshable token
	// fails closed (502) rather than calling the orchestrator unauthenticated.
	proxy := func(method, target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token, err := auth.orchestratorToken(r.Context(), r)
			if err != nil {
				http.Error(w, "auth unavailable", http.StatusBadGateway)
				return
			}
			req, err := http.NewRequestWithContext(r.Context(), method, target, r.Body)
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if ct := r.Header.Get("Content-Type"); ct != "" {
				req.Header.Set("Content-Type", ct)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			copyResponse(w, resp)
		}
	}

	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		auth.require(http.HandlerFunc(auth.handleMe)).ServeHTTP(w, r)
	})

	// Client-side config the static UI reads on load (e.g. the env-specific docs
	// link target). Behind a session like the rest of /api.
	mux.Handle("GET /api/config", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"docsUrl": docsURL})
	})))
	// List — the orchestrator scopes the result to the token's user (no
	// cross-user enumeration); the dashboard no longer supplies userId.
	mux.Handle("GET /api/agents", auth.require(proxy("GET", orchestratorURL+"/v1/agents")))

	// Spawn — the human principal is the token's sub; the orchestrator derives
	// userId from it (a browser cannot spoof it), so we just forward the body.
	mux.Handle("POST /api/spawn", auth.require(proxy("POST", orchestratorURL+"/spawn")))

	// For endpoints with path params, extract and forward. Ownership is enforced
	// at the orchestrator off the token sub, so no userId is carried here.
	mux.Handle("GET /api/agents/{id}/events", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("GET", agentOpTarget(orchestratorURL, r.PathValue("id"), "/events"))(w, r)
	})))
	// Logs route — unlike the generic proxy, this MUST forward the inbound
	// query string (container, sinceTime, tailLines) to the orchestrator.
	mux.Handle("GET /api/agents/{id}/logs", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("GET", logsOpTarget(orchestratorURL, r.PathValue("id"), r.URL.Query()))(w, r)
	})))
	mux.Handle("DELETE /api/agents/{id}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("DELETE", agentOpTarget(orchestratorURL, r.PathValue("id"), ""))(w, r)
	})))
	mux.Handle("POST /api/agents/{id}/message", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", agentOpTarget(orchestratorURL, r.PathValue("id"), "/message"))(w, r)
	})))
	mux.Handle("POST /api/agents/{id}/dismiss", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", agentOpTarget(orchestratorURL, r.PathValue("id"), "/dismiss"))(w, r)
	})))
	// revoke/resume cascade an authorization change down the agent's subtree.
	mux.Handle("POST /api/agents/{id}/revoke", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", agentOpTarget(orchestratorURL, r.PathValue("id"), "/revoke"))(w, r)
	})))
	mux.Handle("POST /api/agents/{id}/resume", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", agentOpTarget(orchestratorURL, r.PathValue("id"), "/resume"))(w, r)
	})))
	mux.Handle("GET /api/templates", auth.require(proxy("GET", orchestratorURL+"/v1/templates")))
	// Template management — forward to the orchestrator (which forwards to the
	// registry). POST/PATCH carry a JSON body; the proxy helper forwards method
	// and body verbatim. PATCH/DELETE build the target per-request from the
	// {agentType} path param (Go 1.22 mux), mirroring the /api/agents/{id} routes.
	mux.Handle("POST /api/templates", auth.require(proxy("POST", orchestratorURL+"/v1/templates")))
	mux.Handle("PATCH /api/templates/{agentType}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("PATCH", orchestratorURL+"/v1/templates/"+url.PathEscape(r.PathValue("agentType")))(w, r)
	})))
	mux.Handle("DELETE /api/templates/{agentType}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("DELETE", orchestratorURL+"/v1/templates/"+url.PathEscape(r.PathValue("agentType")))(w, r)
	})))

	// Stored spawn consents (management view). Scoped at the orchestrator to the
	// token's user — the browser cannot list or revoke another user's grants.
	mux.Handle("GET /api/consents", auth.require(proxy("GET", orchestratorURL+"/v1/consents")))
	mux.Handle("POST /api/consents/{id}/revoke", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", orchestratorURL+"/v1/consents/"+url.PathEscape(r.PathValue("id"))+"/revoke")(w, r)
	})))

	// Pending consent requests (Phase 5b broker). The registry owns the
	// pending->approved/denied lifecycle, so the dashboard banner reads pending
	// requests for the session user and approves/denies them here rather than
	// via IdentityServer's CIBA endpoints. The orchestrator scopes every call to
	// the token's user (confused-deputy protection on approve/deny).
	mux.Handle("GET /api/consent-requests", auth.require(proxy("GET", orchestratorURL+"/v1/consent-requests?status=pending")))
	mux.Handle("POST /api/consent-requests/{id}/approve", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", orchestratorURL+"/v1/consent-requests/"+url.PathEscape(r.PathValue("id"))+"/approve")(w, r)
	})))
	mux.Handle("POST /api/consent-requests/{id}/deny", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", orchestratorURL+"/v1/consent-requests/"+url.PathEscape(r.PathValue("id"))+"/deny")(w, r)
	})))

	return mux
}

// copyResponse copies an upstream response's status, headers, and body verbatim.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// agentOpTarget builds the orchestrator URL for a per-agent op. The agent id is
// PathEscape'd into its path segment so a crafted id can't smuggle a query
// parameter. Ownership is enforced at the orchestrator off the access token's
// sub, so no userId is carried here.
func agentOpTarget(base, id, suffix string) string {
	return base + "/v1/agents/" + url.PathEscape(id) + suffix
}

// logsOpTarget builds the orchestrator logs URL, carrying the inbound logs
// params (container/sinceTime/tailLines, in q). Like agentOpTarget, the id is
// PathEscape'd; ownership is enforced at the orchestrator off the token sub.
func logsOpTarget(base, id string, q url.Values) string {
	if enc := q.Encode(); enc != "" {
		return base + "/v1/agents/" + url.PathEscape(id) + "/logs?" + enc
	}
	return base + "/v1/agents/" + url.PathEscape(id) + "/logs"
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
