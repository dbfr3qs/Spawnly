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

func buildMux(sdb spicedb.Client, validator tokenvalidator.TokenValidator) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /work", func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			http.Error(w, "missing X-Tenant-ID", http.StatusBadRequest)
			return
		}

		rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if rawToken == "" {
			http.Error(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}

		spiffeID, err := validator.ValidateAccessToken(r.Context(), rawToken)
		if err != nil {
			log.Printf("token validation failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		agentName := path.Base(spiffeID)

		allowed, err := sdb.CheckPermission(r.Context(), "tenant:"+tenantID, "work_on", "agent:"+agentName)
		if err != nil {
			log.Printf("spicedb error: %v", err)
			http.Error(w, "authz error", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":    "ok",
			"agentName": agentName,
			"message":   "work accepted",
		})
	})

	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.Header.Get("X-Tenant-ID")
		if tenantID == "" {
			http.Error(w, "missing X-Tenant-ID", http.StatusBadRequest)
			return
		}
		rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if rawToken == "" {
			http.Error(w, "missing Authorization header", http.StatusUnauthorized)
			return
		}
		spiffeID, err := validator.ValidateAccessToken(r.Context(), rawToken)
		if err != nil {
			log.Printf("token validation failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		agentName := path.Base(spiffeID)
		allowed, err := sdb.CheckPermission(r.Context(), "tenant:"+tenantID, "work_on", "agent:"+agentName)
		if err != nil {
			log.Printf("spicedb error: %v", err)
			http.Error(w, "authz error", http.StatusInternalServerError)
			return
		}
		if !allowed {
			http.Error(w, "forbidden", http.StatusForbidden)
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
			"agentName": agentName,
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

	sdb, err := spicedb.New(spicedbEndpoint, spicedbPSK)
	if err != nil {
		log.Fatalf("spicedb connect: %v", err)
	}

	validator, err := tokenvalidator.New(ctx, isIssuer, isJWKSURL)
	if err != nil {
		log.Fatalf("token validator init: %v", err)
	}

	log.Println("sample-api listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", buildMux(sdb, validator)))
}
