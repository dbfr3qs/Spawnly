// cmd/registry/main.go
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/agent-platform/poc/internal/registry"
	"github.com/agent-platform/poc/internal/spicedb"
	"github.com/agent-platform/poc/internal/spiffe"
)

type store struct {
	mu        sync.RWMutex
	templates map[string]registry.AgentTemplate
	agents    map[string]registry.AgentRecord
}

func newStore() *store {
	return &store{
		templates: map[string]registry.AgentTemplate{},
		agents:    map[string]registry.AgentRecord{},
	}
}

func (s *store) putTemplate(t registry.AgentTemplate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.templates[t.AgentType] = t
}

func (s *store) getTemplate(agentType string) (registry.AgentTemplate, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.templates[agentType]
	return t, ok
}

func (s *store) registerAgent(r registry.AgentRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[r.AgentID] = r
}

func (s *store) getAgent(id string) registry.AgentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agents[id]
}

func (s *store) updateAgent(id, status string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.agents[id]
	if !ok {
		return false
	}
	r.Status = status
	s.agents[id] = r
	return true
}

func substitute(tmpl, agentID, tenantID string) string {
	tmpl = strings.ReplaceAll(tmpl, "{{agent_id}}", agentID)
	return strings.ReplaceAll(tmpl, "{{tenant_id}}", tenantID)
}

func buildMux(s *store, sdb spicedb.Client, validator spiffe.SVIDValidator) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		var t registry.AgentTemplate
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.putTemplate(t)
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("GET /v1/templates/", func(w http.ResponseWriter, r *http.Request) {
		agentType := strings.TrimPrefix(r.URL.Path, "/v1/templates/")
		t, ok := s.getTemplate(agentType)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t)
	})

	mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if rawToken == "" {
			http.Error(w, "missing SVID", http.StatusUnauthorized)
			return
		}
		spiffeID, err := validator.Validate(r.Context(), rawToken, "registry")
		if err != nil {
			log.Printf("SVID validation failed: %v", err)
			http.Error(w, "invalid SVID", http.StatusUnauthorized)
			return
		}
		agentID := path.Base(spiffeID)

		var req struct {
			AgentType string `json:"agentType"`
			TenantID  string `json:"tenantId"`
			UserID    string `json:"userId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		tpl, ok := s.getTemplate(req.AgentType)
		if !ok {
			http.Error(w, "unknown agent type", http.StatusBadRequest)
			return
		}

		rec := registry.AgentRecord{
			AgentID:   agentID,
			AgentType: req.AgentType,
			TenantID:  req.TenantID,
			UserID:    req.UserID,
			Status:    "active",
		}
		s.registerAgent(rec)

		for _, rel := range tpl.AuthZ.SpiceDBRelations {
			res := substitute(rel.Resource, agentID, req.TenantID)
			sub := substitute(rel.Subject, agentID, req.TenantID)
			if err := sdb.WriteRelationship(r.Context(), res, rel.Relation, sub); err != nil {
				log.Printf("spicedb write error: %v", err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(rec)
	})

	mux.HandleFunc("GET /v1/agents/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		rec := s.getAgent(agentID)
		if rec.AgentID == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rec)
	})

	mux.HandleFunc("PATCH /v1/agents/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		var req struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if !s.updateAgent(agentID, req.Status) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if req.Status == "completed" || req.Status == "failed" {
			if err := sdb.DeleteAgentRelationships(r.Context(), agentID); err != nil {
				log.Printf("spicedb cleanup error for %s: %v", agentID, err)
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx := context.Background()

	spicedbEndpoint := getEnv("SPICEDB_ENDPOINT", "spicedb:50051")
	spicedbPSK := getEnv("SPICEDB_PSK", "poc-secret")
	spireJWKSURL := getEnv("SPIRE_JWKS_URL", "http://spire-oidc/.well-known/jwks.json")

	sdb, err := spicedb.New(spicedbEndpoint, spicedbPSK)
	if err != nil {
		log.Fatalf("spicedb connect: %v", err)
	}

	validator, err := spiffe.NewJWKSValidator(ctx, spireJWKSURL)
	if err != nil {
		log.Fatalf("SVID validator init: %v", err)
	}

	s := newStore()
	log.Println("registry listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", buildMux(s, sdb, validator)))
}
