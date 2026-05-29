// cmd/sample-api/main.go
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/agent-platform/poc/internal/spicedb"
	"github.com/agent-platform/poc/internal/tokenvalidator"
)

// apiConfig parameterizes a sample-api instance.
type apiConfig struct {
	// audience is the value that an incoming token's `aud` must contain.
	audience string
	// scopePrefix is prepended to ":read"/":write" for scope checks.
	scopePrefix string
}

// authorize validates the bearer token, enforces audience + scope, and checks
// that the acting agent is allowed to work_on behalf of the tenant. It returns
// the validated claims on success, or writes an error response and returns
// ok=false.
func authorize(w http.ResponseWriter, r *http.Request, sdb spicedb.Client, validator tokenvalidator.TokenValidator, cfg apiConfig, requiredScope string) (tokenvalidator.Claims, bool) {
	tenantID := r.Header.Get("X-Tenant-ID")
	if tenantID == "" {
		http.Error(w, "missing X-Tenant-ID", http.StatusBadRequest)
		return tokenvalidator.Claims{}, false
	}

	rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if rawToken == "" {
		http.Error(w, "missing Authorization header", http.StatusUnauthorized)
		return tokenvalidator.Claims{}, false
	}

	claims, err := validator.ValidateAccessToken(r.Context(), rawToken)
	if err != nil {
		log.Printf("token validation failed: %v", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return tokenvalidator.Claims{}, false
	}

	// Reject delegation-only tokens and tokens minted for a different API.
	if claims.TokenUse == "delegation" {
		log.Printf("rejecting token with token_use=delegation")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return tokenvalidator.Claims{}, false
	}
	if !claims.HasAudience(cfg.audience) {
		log.Printf("rejecting token: aud %v does not contain %q", claims.Audience, cfg.audience)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return tokenvalidator.Claims{}, false
	}

	if !claims.HasScope(requiredScope) {
		log.Printf("rejecting token: missing scope %q (have %v)", requiredScope, claims.Scopes)
		http.Error(w, "forbidden", http.StatusForbidden)
		return tokenvalidator.Claims{}, false
	}

	// Authorize the ENTIRE delegation chain, not just the acting agent: every
	// agent in the act chain must still hold work_on on the tenant. Suspending
	// any ancestor removes its work_on grant, so this cascades to deny every
	// descendant's in-flight token. The chain is bounded by the delegation maxDepth.
	deniedMember, ok, err := authorizeChain(r.Context(), sdb, tenantID, claims.Chain)
	if err != nil {
		log.Printf("spicedb error: %v", err)
		http.Error(w, "authz error", http.StatusInternalServerError)
		return tokenvalidator.Claims{}, false
	}
	if !ok {
		log.Printf("chain authorization denied: member %q lacks work_on on tenant:%s", deniedMember, tenantID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return tokenvalidator.Claims{}, false
	}

	return claims, true
}

// authorizeChain returns ok=true only if every agent in the delegation chain
// currently holds work_on on the tenant. On the first member that does not, it
// returns that member's id and ok=false (cascade denial for a suspended ancestor).
func authorizeChain(ctx context.Context, sdb spicedb.Client, tenantID string, chain []string) (deniedMember string, ok bool, err error) {
	for _, spiffeID := range chain {
		id := path.Base(spiffeID)
		allowed, err := sdb.CheckPermission(ctx, "tenant:"+tenantID, "work_on", "agent:"+id)
		if err != nil {
			return "", false, err
		}
		if !allowed {
			return id, false, nil
		}
	}
	return "", true, nil
}

func buildMux(sdb spicedb.Client, validator tokenvalidator.TokenValidator, cfg apiConfig) *http.ServeMux {
	mux := http.NewServeMux()

	readScope := cfg.scopePrefix + ":read"
	writeScope := cfg.scopePrefix + ":write"

	mux.HandleFunc("GET /work", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authorize(w, r, sdb, validator, cfg, readScope)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"agentName": claims.ActingAgentName,
			"message":   "work accepted",
		})
	})

	mux.HandleFunc("POST /work", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authorize(w, r, sdb, validator, cfg, writeScope)
		if !ok {
			return
		}
		var req struct {
			Task string `json:"task"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"task":      req.Task,
			"result":    "echo: " + req.Task,
			"agentName": claims.ActingAgentName,
		})
	})

	// POST /task is retained for backward compatibility with existing callers.
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authorize(w, r, sdb, validator, cfg, writeScope)
		if !ok {
			return
		}
		var req struct {
			Task string `json:"task"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"task":      req.Task,
			"result":    "echo: " + req.Task,
			"agentName": claims.ActingAgentName,
		})
	})

	return mux
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()
	spicedbEndpoint := getenv("SPICEDB_ENDPOINT", "spicedb:50051")
	spicedbPSK := getenv("SPICEDB_PSK", "poc-secret")
	isIssuer := getenv("IS_ISSUER", "http://identity-server")
	isJWKSURL := getenv("IS_JWKS_URL", "http://identity-server/.well-known/openid-configuration/jwks")

	audience := getenv("API_AUDIENCE", "sample-api")
	scopePrefix := getenv("SCOPE_PREFIX", audience)
	cfg := apiConfig{audience: audience, scopePrefix: scopePrefix}

	sdb, err := spicedb.New(spicedbEndpoint, spicedbPSK)
	if err != nil {
		log.Fatalf("spicedb connect: %v", err)
	}

	validator, err := tokenvalidator.New(ctx, isIssuer, isJWKSURL)
	if err != nil {
		log.Fatalf("token validator init: %v", err)
	}

	log.Printf("sample-api listening on :8080 (audience=%s, scopePrefix=%s)", audience, scopePrefix)
	log.Fatal(http.ListenAndServe(":8080", buildMux(sdb, validator, cfg)))
}
