package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
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

	// OIDC config. Authority is the browser-facing origin; identityInternalURL is
	// the in-cluster back-channel target — the Keycloak realm issuer
	// (http://keycloak:8080/realms/spawnly), which is also the token issuer the
	// resource servers validate against.
	authority := getenv("OIDC_AUTHORITY", "http://localhost:8090")
	identityInternalURL := getenv("IDENTITY_INTERNAL_URL", "http://keycloak:8080/realms/spawnly")
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

	// Reverse-proxy the browser-facing OIDC endpoints to Keycloak so the browser
	// only ever talks to the dashboard origin (single issuer). These are public —
	// they must not be gated by require().
	//
	// IDENTITY_INTERNAL_URL is the realm issuer (http://keycloak:8080/realms/
	// spawnly), but the browser-facing paths (/realms/<realm>/..., /resources/...)
	// are absolute from Keycloak's ROOT, so we proxy to the host (scheme+host)
	// only — stripping the /realms/spawnly path — and forward the request path
	// verbatim. Using the issuer URL as the proxy target would double the
	// /realms/spawnly prefix.
	idHost, err := url.Parse(identityInternalURL)
	if err != nil {
		log.Fatalf("invalid IDENTITY_INTERNAL_URL: %v", err)
	}
	idHost.Path, idHost.RawQuery, idHost.Fragment = "", "", ""
	idProxy := httputil.NewSingleHostReverseProxy(idHost)
	// PRESERVE the inbound browser Host on the proxied request. Keycloak runs with
	// dynamic hostname (KC_HOSTNAME_STRICT=false, no fixed KC_HOSTNAME), so it
	// derives the login page's form action + asset URLs and the frontend issuer
	// from the request Host. By forwarding the browser's Host (the dashboard
	// origin) instead of the upstream keycloak:8080, those URLs land on the
	// dashboard origin and the browser stays single-origin — while backchannel
	// calls (discovery/token/JWKS, made directly by the dashboard/resource
	// servers to keycloak:8080) keep the internal issuer. Without this, Keycloak
	// would emit an absolute http://keycloak:8080/... login action the browser
	// can't reach. The default SingleHostReverseProxy director rewrites only
	// URL.Scheme/Host, so we set req.Host explicitly to be robust across Go
	// versions.
	baseDirector := idProxy.Director
	idProxy.Director = func(req *http.Request) {
		inboundHost := req.Host
		baseDirector(req)
		req.Host = inboundHost
		// Strip client-controlled forwarding headers. We drive Keycloak's frontend
		// URLs purely via the preserved Host (above); if the deploy ever sets
		// KC_PROXY_HEADERS=xforwarded (the future ALB switch), a browser-forged
		// X-Forwarded-Host / Forwarded could otherwise steer Keycloak's generated
		// issuer/redirect/asset URLs to an attacker origin. Deleting them here means
		// the future flag can only ever trust headers a real upstream proxy sets,
		// never ones a client smuggled through this proxy.
		req.Header.Del("X-Forwarded-Host")
		req.Header.Del("X-Forwarded-Proto")
		req.Header.Del("X-Forwarded-Port")
		req.Header.Del("Forwarded")
	}
	// ALLOWLIST — forward ONLY Keycloak's browser-facing login/OIDC surface:
	//   /realms/<realm>/ — authorize, token, logout, JWKS, discovery, login POST,
	//                      for THIS realm only. The realm segment is derived from
	//                      IDENTITY_INTERNAL_URL (realmPath()) so it stays in sync
	//                      with the issuer; other realms — notably /realms/master/
	//                      and its internet-reachable direct-grant token endpoint —
	//                      are NOT proxied and fall through to the session-gated
	//                      "/" handler (upstream Keycloak never hit).
	//   /resources/      — Keycloak's login-theme static assets (CSS/JS/images);
	//                      not realm-scoped in Keycloak's URL space.
	// Everything else (notably /admin and /admin/** — the Keycloak admin console
	// and admin REST API) is DELIBERATELY NOT proxied, so the admin console is
	// unreachable through the dashboard origin. Adding a prefix here widens that
	// boundary — do not add /admin.
	//
	// realmPath() is e.g. "/realms/spawnly"; register the trailing-slash prefix so
	// only that realm's subtree matches. Fall back to "/realms/" only if the realm
	// couldn't be derived (misconfigured IDENTITY_INTERNAL_URL surfaces at
	// discovery anyway) rather than proxying nothing and breaking login.
	realmPrefix := "/realms/"
	if rp := auth.realmPath(); rp != "" {
		realmPrefix = rp + "/"
	}
	mux.Handle(realmPrefix, idProxy)
	mux.Handle("/resources/", idProxy)
	// Pending spawn-consent is served by the registry-native broker via the
	// /api/consent-requests routes below (Phase 5b); Keycloak's own consent UI is
	// not used.

	// Relying-party routes (public). /signin starts the OIDC code+PKCE flow
	// (require() redirects here); the browser then rests on the /login form above.
	mux.HandleFunc("GET /signin", auth.handleLogin)
	mux.HandleFunc("GET /callback", auth.handleCallback)
	mux.HandleFunc("POST /logout", auth.handleLogout)
	// Switch the active org via a full re-auth (re-auth-per-org). Public like
	// /signin: it clears the current session and restarts the OIDC flow with the
	// new org scope; Keycloak SSO makes the re-auth password-less.
	mux.HandleFunc("GET /switch-org", auth.handleSwitchOrg)

	// Static UI behind a session (browser GETs redirect to /signin when absent).
	// Method-less so it doesn't conflict with the method-less OIDC proxy
	// prefixes (Go 1.22 mux rejects a "GET /" catch-all alongside "/connect/").
	mux.Handle("/", auth.require(http.FileServer(http.FS(staticFS))))

	// Proxy handlers — forward to orchestrator, copy status+headers+body verbatim.
	// Every orchestrator call carries the session's delegated access token as
	// `Authorization: Bearer` (the orchestrator derives userId + scopes from it).
	// We send CLEAN headers — only Content-Type and the bearer — so the browser's
	// own cookies/Authorization never leak upstream. We fail closed rather than
	// calling the orchestrator unauthenticated: an expired/absent session is the
	// caller's problem (401, so the UI can re-drive login), an unrefreshable token
	// is ours (502).
	proxy := func(method, target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			token, err := auth.orchestratorToken(r.Context(), r)
			if errors.Is(err, errNoSession) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
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

	// /api/me drives the org UX: the UI reads it on load to pick between the
	// dashboard, the org picker, and the no-organization screen. It composes the
	// session-derived identity (user/isAdmin/activeOrg) with the caller's org
	// memberships fetched from the orchestrator's GET /v1/my/orgs (which keys on the
	// session token's sub — the browser cannot ask about another user). On an
	// orchestrator hiccup the org fields DEGRADE to empty with orgsUnavailable=true
	// so the UI can show a soft retry rather than a false "no organization" screen.
	mux.Handle("GET /api/me", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := auth.user(r)
		out := map[string]any{
			"user":       user,
			"isAdmin":    auth.isAdmin(r),
			"activeOrg":  auth.activeOrg(r),
			"orgs":       []any{},
			"isOrgAdmin": false,
		}
		token, err := auth.orchestratorToken(r.Context(), r)
		if err != nil {
			log.Printf("api/me: no orchestrator token for orgs fetch: %v", err)
			out["orgsUnavailable"] = true
		} else if my, ferr := fetchMyOrgs(r.Context(), orchestratorURL, token); ferr != nil {
			log.Printf("api/me: fetch my orgs: %v", ferr)
			out["orgsUnavailable"] = true
		} else {
			out["orgs"] = my.Orgs
			out["isOrgAdmin"] = my.IsOrgAdmin
			// activeOrg is taken from the session (parsed from the same access token);
			// my.ActiveOrg agrees. Keeping the session value avoids a second source.
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})))

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
	// Non-admin spawn list ({agentType, requiresTenant}) for the spawn modal to
	// decide whether to show the Tenant field. Same non-admin gate as
	// GET /api/templates; the target is a fixed orchestrator path so no client
	// query can redirect it at the admin-only full-detail list.
	mux.Handle("GET /api/templates/spawn", auth.require(proxy("GET", orchestratorURL+"/v1/templates/spawn")))
	// Admin full-detail template list (incl. disabled) for the Agent Types
	// admin view. Admin-gated at the BFF (defense in depth with the
	// orchestrator's GET /v1/admin/templates); a non-admin session gets 403 here
	// before the orchestrator is reached.
	mux.Handle("GET /api/admin/templates", auth.requireAdmin(proxy("GET", orchestratorURL+"/v1/admin/templates")))
	// Template management — forward to the orchestrator (which forwards to the
	// registry). SESSION-gated only at this BFF tier (Phase 6): the real authz is
	// the orchestrator's requireTemplateManager, which admits a REALM admin (global
	// authority) OR an ORG admin (scoped to their org) and stamps the org from the
	// token — neither of which the stateless BFF can evaluate (it has no SpiceDB),
	// so gating on realm-admin here would wrongly lock out org admins. POST/PATCH
	// carry a JSON body; the proxy helper forwards method and body verbatim.
	// PATCH/DELETE build the target per-request from the {agentType} path param.
	mux.Handle("POST /api/templates", auth.require(proxy("POST", orchestratorURL+"/v1/templates")))
	mux.Handle("PATCH /api/templates/{agentType}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("PATCH", orchestratorURL+"/v1/templates/"+url.PathEscape(r.PathValue("agentType")))(w, r)
	})))
	mux.Handle("DELETE /api/templates/{agentType}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("DELETE", orchestratorURL+"/v1/templates/"+url.PathEscape(r.PathValue("agentType")))(w, r)
	})))

	// Org onboarding + membership admin API (multi-tenancy Phase 3). Platform-admin
	// only: requireAdmin gates every twin at this BFF tier (defense in depth with
	// the orchestrator's requireAdmin). The proxy carries the session's delegated
	// access token; the orchestrator forwards to the registry with its
	// control-plane bearer. Org-admin self-service twins (/api/org/...) come in
	// Phase 6.
	mux.Handle("POST /api/admin/orgs", auth.requireAdmin(proxy("POST", orchestratorURL+"/v1/admin/orgs")))
	mux.Handle("GET /api/admin/orgs", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := orchestratorURL + "/v1/admin/orgs"
		if m := r.URL.Query().Get("member"); m != "" {
			target += "?member=" + url.QueryEscape(m)
		}
		proxy("GET", target)(w, r)
	})))
	mux.Handle("GET /api/admin/orgs/{id}", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("GET", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id")))(w, r)
	})))
	mux.Handle("PATCH /api/admin/orgs/{id}", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("PATCH", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id")))(w, r)
	})))
	mux.Handle("DELETE /api/admin/orgs/{id}", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("DELETE", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id")))(w, r)
	})))
	mux.Handle("GET /api/admin/orgs/{id}/members", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("GET", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id"))+"/members")(w, r)
	})))
	mux.Handle("POST /api/admin/orgs/{id}/members", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id"))+"/members")(w, r)
	})))
	mux.Handle("PATCH /api/admin/orgs/{id}/members/{user}", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("PATCH", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id"))+"/members/"+url.PathEscape(r.PathValue("user")))(w, r)
	})))
	mux.Handle("DELETE /api/admin/orgs/{id}/members/{user}", auth.requireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("DELETE", orchestratorURL+"/v1/admin/orgs/"+url.PathEscape(r.PathValue("id"))+"/members/"+url.PathEscape(r.PathValue("user")))(w, r)
	})))

	// Org SELF-SERVICE twins (multi-tenancy Phase 6). These are the ORG ADMIN's
	// membership + settings surface for their OWN org — the org is taken from the
	// SESSION's active org (never a client-supplied id), so the browser can only
	// ever act on the org it is logged into. The orchestrator re-derives the org
	// from the same token and enforces requireOrgAdmin/requireOrgMember (the real
	// gate); this BFF layer is a session-gated convenience. An org-less session
	// (no active org) gets 404 — nothing to self-manage. orgTarget builds the
	// orchestrator path for the active org, or "" when there is none.
	orgTarget := func(r *http.Request, suffix string) string {
		org := auth.activeOrg(r)
		if org == "" {
			return ""
		}
		return orchestratorURL + "/v1/orgs/" + url.PathEscape(org) + suffix
	}
	orgProxy := func(method, suffix string) http.Handler {
		return auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			target := orgTarget(r, suffix)
			if target == "" {
				http.Error(w, "no active organization", http.StatusNotFound)
				return
			}
			proxy(method, target)(w, r)
		}))
	}
	// Org record (name, removal disposition, anchor) — members may read; only
	// admins may PATCH settings (the orchestrator enforces the split).
	mux.Handle("GET /api/org", orgProxy("GET", ""))
	mux.Handle("PATCH /api/org", orgProxy("PATCH", ""))
	// Membership: list (member), add/remove/promote-demote (admin).
	mux.Handle("GET /api/org/members", orgProxy("GET", "/members"))
	mux.Handle("POST /api/org/members", orgProxy("POST", "/members"))
	mux.Handle("PATCH /api/org/members/{user}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := orgTarget(r, "/members/"+url.PathEscape(r.PathValue("user")))
		if target == "" {
			http.Error(w, "no active organization", http.StatusNotFound)
			return
		}
		proxy("PATCH", target)(w, r)
	})))
	mux.Handle("DELETE /api/org/members/{user}", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := orgTarget(r, "/members/"+url.PathEscape(r.PathValue("user")))
		if target == "" {
			http.Error(w, "no active organization", http.StatusNotFound)
			return
		}
		proxy("DELETE", target)(w, r)
	})))
	// Org-admin Agent Types view: the org's own templates (detail=full, scoped to
	// the active org at the orchestrator). The orchestrator derives the org from the
	// token, so no path param is needed here.
	mux.Handle("GET /api/org/templates", auth.require(proxy("GET", orchestratorURL+"/v1/org/templates")))

	// Stored spawn consents (management view). Scoped at the orchestrator to the
	// token's user — the browser cannot list or revoke another user's grants.
	mux.Handle("GET /api/consents", auth.require(proxy("GET", orchestratorURL+"/v1/consents")))
	mux.Handle("POST /api/consents/{id}/revoke", auth.require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxy("POST", orchestratorURL+"/v1/consents/"+url.PathEscape(r.PathValue("id"))+"/revoke")(w, r)
	})))

	// Pending consent requests (Phase 5b broker). The registry owns the
	// pending->approved/denied lifecycle, so the dashboard banner reads pending
	// requests for the session user and approves/denies them here rather than
	// via Keycloak's CIBA endpoints. The orchestrator scopes every call to
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

// myOrgs is the shape of the orchestrator's GET /v1/my/orgs response. Orgs stays a
// list of raw objects (orgId/name/…) so the org fields pass through to the UI
// without the dashboard needing the registry's Organization type.
type myOrgs struct {
	Orgs       []map[string]any `json:"orgs"`
	ActiveOrg  string           `json:"activeOrg"`
	IsOrgAdmin bool             `json:"isOrgAdmin"`
}

// fetchMyOrgs calls the orchestrator's session-gated GET /v1/my/orgs with the
// session's delegated token. The orchestrator keys the membership lookup on the
// token sub, so the result is exactly the caller's own orgs.
func fetchMyOrgs(ctx context.Context, orchestratorURL, token string) (myOrgs, error) {
	var out myOrgs
	req, err := http.NewRequestWithContext(ctx, "GET", orchestratorURL+"/v1/my/orgs", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("orchestrator /v1/my/orgs: status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
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
