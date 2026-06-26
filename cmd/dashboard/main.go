package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
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

	// Proxy handlers — forward to orchestrator, copy status+headers+body verbatim
	proxy := func(method, target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), method, target, r.Body)
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			req.Header = r.Header.Clone()
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
	mux.Handle("GET /api/agents", auth.require(proxy("GET", orchestratorURL+"/v1/agents")))

	// Spawn — inject the authenticated user's identity as userId so the human
	// principal (not a browser-supplied value) flows into the agent's token sub.
	mux.Handle("POST /api/spawn", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r) // require() guarantees a session
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		body, err = injectUserID(body, user)
		if err != nil {
			http.Error(w, "bad request body", http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "POST", orchestratorURL+"/spawn", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Header = r.Header.Clone()
		req.ContentLength = int64(len(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		copyResponse(w, resp)
	})))

	// For endpoints with path params, extract and forward
	mux.Handle("GET /api/agents/{id}/events", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("GET", orchestratorURL+"/v1/agents/"+id+"/events")(w, r)
	})))
	// Logs route — unlike the generic proxy, this MUST forward the inbound
	// query string (container, sinceTime, tailLines) to the orchestrator.
	mux.Handle("GET /api/agents/{id}/logs", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		target := orchestratorURL + "/v1/agents/" + id + "/logs"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(r.Context(), "GET", target, nil)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "upstream error", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		copyResponse(w, resp)
	})))
	mux.Handle("DELETE /api/agents/{id}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("DELETE", orchestratorURL+"/v1/agents/"+id)(w, r)
	})))
	mux.Handle("POST /api/agents/{id}/message", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/agents/"+id+"/message")(w, r)
	})))
	mux.Handle("POST /api/agents/{id}/dismiss", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/agents/"+id+"/dismiss")(w, r)
	})))
	// revoke/resume cascade an authorization change down the agent's subtree.
	mux.Handle("POST /api/agents/{id}/revoke", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/agents/"+id+"/revoke")(w, r)
	})))
	mux.Handle("POST /api/agents/{id}/resume", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/agents/"+id+"/resume")(w, r)
	})))
	mux.Handle("GET /api/templates", auth.require(proxy("GET", orchestratorURL+"/v1/templates")))
	// Template management — forward to the orchestrator (which forwards to the
	// registry). POST/PATCH carry a JSON body; the proxy helper forwards method
	// and body verbatim. PATCH/DELETE build the target per-request from the
	// {agentType} path param (Go 1.22 mux), mirroring the /api/agents/{id} routes.
	mux.Handle("POST /api/templates", auth.require(proxy("POST", orchestratorURL+"/v1/templates")))
	mux.Handle("PATCH /api/templates/{agentType}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentType := r.PathValue("agentType")
		proxy("PATCH", orchestratorURL+"/v1/templates/"+agentType)(w, r)
	})))
	mux.Handle("DELETE /api/templates/{agentType}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentType := r.PathValue("agentType")
		proxy("DELETE", orchestratorURL+"/v1/templates/"+agentType)(w, r)
	})))

	// Stored spawn consents (management view). Scoped server-side to the
	// session user — the browser cannot list or revoke another user's grants.
	mux.Handle("GET /api/consents", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r)
		proxy("GET", orchestratorURL+"/v1/consents?userId="+url.QueryEscape(user))(w, r)
	})))
	mux.Handle("POST /api/consents/{id}/revoke", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r)
		id := r.PathValue("id")
		// userId scopes the registry lookup to the session user's own records,
		// so a guessed consent id belonging to someone else 404s.
		proxy("POST", orchestratorURL+"/v1/consents/"+id+"/revoke?userId="+url.QueryEscape(user))(w, r)
	})))

	// Pending consent requests (Phase 5b broker). The registry owns the
	// pending->approved/denied lifecycle, so the dashboard banner reads pending
	// requests for the session user and approves/denies them here rather than
	// via IdentityServer's CIBA endpoints. userId scopes every call to the
	// session user (confused-deputy protection on approve/deny).
	mux.Handle("GET /api/consent-requests", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r)
		proxy("GET", orchestratorURL+"/v1/consent-requests?status=pending&userId="+url.QueryEscape(user))(w, r)
	})))
	mux.Handle("POST /api/consent-requests/{id}/approve", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r)
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/consent-requests/"+id+"/approve?userId="+url.QueryEscape(user))(w, r)
	})))
	mux.Handle("POST /api/consent-requests/{id}/deny", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r)
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/consent-requests/"+id+"/deny?userId="+url.QueryEscape(user))(w, r)
	})))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("dashboard listening on :%s (orchestrator: %s, oidc authority: %s)", port, orchestratorURL, authority)
	log.Fatal(http.ListenAndServe(":"+port, mux))
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

// injectUserID overwrites the spawn request's userId with the authenticated
// user, so the human identity is authoritative (a browser cannot spoof it).
func injectUserID(body []byte, user string) ([]byte, error) {
	var m map[string]any
	if len(body) == 0 {
		m = map[string]any{}
	} else if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	m["userId"] = user
	return json.Marshal(m)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
