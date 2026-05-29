// cmd/registry/main.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/agent-platform/poc/internal/events"
	"github.com/agent-platform/poc/internal/registry"
	"github.com/agent-platform/poc/internal/spicedb"
	"github.com/agent-platform/poc/internal/spiffe"
)

type store struct {
	mu        sync.RWMutex
	templates map[string]registry.AgentTemplate
	agents    map[string]registry.AgentRecord
	events    map[string][]events.Event
}

func newStore() *store {
	return &store{
		templates: map[string]registry.AgentTemplate{},
		agents:    map[string]registry.AgentRecord{},
		events:    map[string][]events.Event{},
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

func (s *store) appendEvent(agentID string, e events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	e.Timestamp = time.Now()
	s.events[agentID] = append(s.events[agentID], e)
}

func (s *store) getEvents(agentID string) []events.Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src := s.events[agentID]
	out := make([]events.Event, len(src))
	copy(out, src)
	return out
}

func (s *store) listAgents() []registry.AgentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]registry.AgentRecord, 0, len(s.agents))
	for _, r := range s.agents {
		if !r.Dismissed {
			out = append(out, r)
		}
	}
	return out
}

func (s *store) dismissAgent(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.agents[id]
	if !ok {
		return false
	}
	r.Dismissed = true
	s.agents[id] = r
	return true
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

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
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

	// Internal pre-registration endpoint — no SVID required.
	// Called by the orchestrator at spawn time so the agent appears in the UI
	// immediately with "pending" status rather than waiting for the sidecar to start.
	mux.HandleFunc("POST /v1/agents/preregister", func(w http.ResponseWriter, r *http.Request) {
		var rec registry.AgentRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rec.AgentID == "" {
			http.Error(w, "agentId required", http.StatusBadRequest)
			return
		}
		rec.Status = "pending"
		s.registerAgent(rec)
		s.appendEvent(rec.AgentID, events.Event{
			Source:  events.SourceOrchestrator,
			Type:    "workload_spawning",
			Payload: mustMarshal(map[string]string{"agentId": rec.AgentID, "agentType": rec.AgentType}),
		})
		w.WriteHeader(http.StatusCreated)
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
			ParentID  string `json:"parentId"`
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
			Lifecycle: tpl.Runtime.Lifecycle,
			ParentID:  req.ParentID,
		}
		s.registerAgent(rec)
		s.appendEvent(agentID, events.Event{
			Source:  events.SourceRegistry,
			Type:    "registry_record_created",
			Payload: mustMarshal(rec),
		})

		type relTuple struct {
			Resource string `json:"resource"`
			Relation string `json:"relation"`
			Subject  string `json:"subject"`
		}
		for _, rel := range tpl.AuthZ.SpiceDBRelations {
			res := substitute(rel.Resource, agentID, req.TenantID)
			sub := substitute(rel.Subject, agentID, req.TenantID)
			if err := sdb.WriteRelationship(r.Context(), res, rel.Relation, sub); err != nil {
				log.Printf("spicedb write error: %v", err)
			}
		}
		tuples := make([]relTuple, len(tpl.AuthZ.SpiceDBRelations))
		for i, rel := range tpl.AuthZ.SpiceDBRelations {
			tuples[i] = relTuple{
				Resource: substitute(rel.Resource, agentID, req.TenantID),
				Relation: rel.Relation,
				Subject:  substitute(rel.Subject, agentID, req.TenantID),
			}
		}
		s.appendEvent(agentID, events.Event{
			Source:  events.SourceRegistry,
			Type:    "spicedb_relations_written",
			Payload: mustMarshal(tuples),
		})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(rec)
	})

	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		agents := s.listAgents()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(agents)
	})

	mux.HandleFunc("POST /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		var e events.Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.appendEvent(agentID, e)
		evts := s.getEvents(agentID)
		stored := evts[len(evts)-1]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(stored)
	})

	mux.HandleFunc("GET /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		evts := s.getEvents(agentID)
		if evts == nil {
			evts = []events.Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(evts)
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

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if !s.dismissAgent(agentID) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Suspend is the revocation primitive: drop the agent's SpiceDB authorization
	// so any check for it (or for a delegation chain that includes it) denies, and
	// mark the record "suspended" so the registry's own logic stops treating it as
	// active (e.g. IsActive, exchange chain checks).
	mux.HandleFunc("POST /v1/agents/{id}/suspend", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		rec := s.getAgent(agentID)
		if rec.AgentID == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		s.updateAgent(agentID, "suspended")
		if err := sdb.DeleteAgentRelationships(r.Context(), agentID); err != nil {
			log.Printf("spicedb cleanup error for %s: %v", agentID, err)
		}
		s.appendEvent(agentID, events.Event{
			Source:  events.SourceRegistry,
			Type:    "agent_suspended",
			Payload: mustMarshal(map[string]string{"agentId": agentID}),
		})
		w.WriteHeader(http.StatusOK)
	})

	// Resume reverses a suspend: re-derive the agent's SpiceDB relations from its
	// template (identical logic to registration) and mark the record active again.
	mux.HandleFunc("POST /v1/agents/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		rec := s.getAgent(agentID)
		if rec.AgentID == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if rec.Status != "suspended" {
			http.Error(w, "agent is not suspended", http.StatusConflict)
			return
		}
		tpl, ok := s.getTemplate(rec.AgentType)
		if !ok {
			http.Error(w, "unknown agent type", http.StatusBadRequest)
			return
		}
		s.updateAgent(agentID, "active")
		for _, rel := range tpl.AuthZ.SpiceDBRelations {
			res := substitute(rel.Resource, agentID, rec.TenantID)
			sub := substitute(rel.Subject, agentID, rec.TenantID)
			if err := sdb.WriteRelationship(r.Context(), res, rel.Relation, sub); err != nil {
				log.Printf("spicedb resume write error for %s: %v", agentID, err)
			}
		}
		s.appendEvent(agentID, events.Event{
			Source:  events.SourceRegistry,
			Type:    "agent_resumed",
			Payload: mustMarshal(map[string]string{"agentId": agentID}),
		})
		w.WriteHeader(http.StatusOK)
	})

	// Delegation policy decision for parentType delegating to childType.
	// Always returns HTTP 200 — a missing/unconfigured parent template yields allowed:false.
	mux.HandleFunc("GET /v1/delegation-policy", func(w http.ResponseWriter, r *http.Request) {
		parentType := r.URL.Query().Get("parentType")
		childType := r.URL.Query().Get("childType")

		resp := struct {
			Allowed         bool     `json:"allowed"`
			GrantableScopes []string `json:"grantableScopes"`
			MaxDepth        int      `json:"maxDepth"`
		}{Allowed: false, GrantableScopes: []string{}, MaxDepth: 0}

		tpl, ok := s.getTemplate(parentType)
		if ok {
			pol := tpl.Delegation
			for _, ct := range pol.AllowedChildTypes {
				if ct == childType {
					resp.Allowed = true
					break
				}
			}
			if resp.Allowed {
				resp.GrantableScopes = pol.GrantableScopes
				if resp.GrantableScopes == nil {
					resp.GrantableScopes = []string{}
				}
				resp.MaxDepth = pol.MaxDepth
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// Delegation lineage for an agent: the agent itself up to the root via ParentID.
	mux.HandleFunc("GET /v1/agents/{id}/chain", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		rec := s.getAgent(agentID)
		if rec.AgentID == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		type chainNode struct {
			AgentID   string `json:"agentId"`
			AgentType string `json:"agentType"`
			Status    string `json:"status"`
			ParentID  string `json:"parentId"`
		}

		chain := []chainNode{}
		seen := map[string]bool{}
		cur := rec
		for i := 0; i < 32; i++ {
			if seen[cur.AgentID] {
				break // cycle guard
			}
			seen[cur.AgentID] = true
			chain = append(chain, chainNode{
				AgentID:   cur.AgentID,
				AgentType: cur.AgentType,
				Status:    cur.Status,
				ParentID:  cur.ParentID,
			})
			if cur.ParentID == "" {
				break
			}
			parent := s.getAgent(cur.ParentID)
			if parent.AgentID == "" {
				break // missing parent — include what's resolvable
			}
			cur = parent
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"chain": chain})
	})

	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		types := make([]string, 0, len(s.templates))
		for k := range s.templates {
			types = append(types, k)
		}
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(types)
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
