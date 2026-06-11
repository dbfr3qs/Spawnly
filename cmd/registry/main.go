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

	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

type store struct {
	mu        sync.RWMutex
	templates map[string]registry.AgentTemplate
	agents    map[string]registry.AgentRecord
	events    map[string][]events.Event
	// consents is keyed by the (user, parentType, childType) edge — one record
	// per edge, replaced on each fresh grant. See consentKey.
	consents map[string]registry.ConsentRecord
}

func newStore() *store {
	return &store{
		templates: map[string]registry.AgentTemplate{},
		agents:    map[string]registry.AgentRecord{},
		events:    map[string][]events.Event{},
		consents:  map[string]registry.ConsentRecord{},
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

func consentKey(userID, parentType, childType string) string {
	return userID + "|" + parentType + "|" + childType
}

// upsertConsent stores a fresh grant for its (user, parentType, childType)
// edge, replacing any previous record (including a revoked one — a new grant
// is the user re-approving). The record's ID is stable across re-grants.
func (s *store) upsertConsent(rec registry.ConsentRecord) registry.ConsentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := consentKey(rec.UserID, rec.ParentType, rec.ChildType)
	if existing, ok := s.consents[key]; ok {
		rec.ID = existing.ID
	} else {
		rec.ID = fmt.Sprintf("consent-%d", time.Now().UnixNano())
	}
	s.consents[key] = rec
	return rec
}

func (s *store) findConsent(userID, parentType, childType string) (registry.ConsentRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.consents[consentKey(userID, parentType, childType)]
	return rec, ok
}

// listConsents returns all consent records, or only one user's when userID is
// non-empty. Revoked records are included (the dashboard shows them as such).
func (s *store) listConsents(userID string) []registry.ConsentRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []registry.ConsentRecord{}
	for _, rec := range s.consents {
		if userID == "" || rec.UserID == userID {
			out = append(out, rec)
		}
	}
	return out
}

// revokeConsent marks the consent with the given id revoked. A non-empty
// userID scopes the lookup to that user's own records — the dashboard path
// always passes the session user, so one user cannot revoke another's grant.
func (s *store) revokeConsent(id, userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, rec := range s.consents {
		if rec.ID == id && (userID == "" || rec.UserID == userID) {
			rec.Revoked = true
			s.consents[key] = rec
			return true
		}
	}
	return false
}

// consentExpiry derives a fresh consent record's expiry from the parent
// template's per-child consentTTL. Nil means the consent never expires —
// also the fallback for an unset or unparsable TTL.
func (s *store) consentExpiry(parentType, childType string, from time.Time) *time.Time {
	tpl, ok := s.getTemplate(parentType)
	if !ok {
		return nil
	}
	cp, ok := tpl.Delegation.ChildPolicies[childType]
	if !ok || cp.ConsentTTL == "" {
		return nil
	}
	d, err := time.ParseDuration(cp.ConsentTTL)
	if err != nil {
		log.Printf("template %s: bad consentTTL %q for child %s: %v", parentType, cp.ConsentTTL, childType, err)
		return nil
	}
	t := from.Add(d)
	return &t
}

// subtree returns the agent id plus every descendant reachable through ParentID
// edges (everything the agent spawned, transitively), root first followed by a
// breadth-first walk. It is the set a cascading revoke/resume operates on.
// Returns nil if the id is unknown.
func (s *store) subtree(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.agents[id]; !ok {
		return nil
	}
	children := map[string][]string{}
	for cid, rec := range s.agents {
		if rec.ParentID != "" {
			children[rec.ParentID] = append(children[rec.ParentID], cid)
		}
	}
	out := []string{}
	seen := map[string]bool{}
	queue := []string{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if seen[cur] {
			continue // cycle guard
		}
		seen[cur] = true
		out = append(out, cur)
		queue = append(queue, children[cur]...)
	}
	return out
}

// depth returns how many agents are in the lineage from id up to the root,
// counting id itself: a top-level agent is depth 1, its child depth 2, and so
// on. Used to enforce a template's maxDepth at spawn time. Unknown ids are 0.
func (s *store) depth(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := 0
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		rec, ok := s.agents[cur]
		if !ok {
			break
		}
		seen[cur] = true // cycle guard
		d++
		cur = rec.ParentID
	}
	return d
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func substitute(tmpl, agentID, tenantID string) string {
	tmpl = strings.ReplaceAll(tmpl, "{{agent_id}}", agentID)
	return strings.ReplaceAll(tmpl, "{{tenant_id}}", tenantID)
}

// referencesTenant reports whether a relation template depends on the tenant
// id. Such relations are skipped for global (tenant-less) agents so we never
// write a malformed "tenant:" tuple.
func referencesTenant(rel registry.SpiceDBRelationTemplate) bool {
	return strings.Contains(rel.Resource, "{{tenant_id}}") || strings.Contains(rel.Subject, "{{tenant_id}}")
}

// revokeNode is the per-agent core of revocation, shared by the cascading revoke
// endpoint: drop the agent's SpiceDB authorization and mark the record "revoked"
// so any permission check denies in real time. The pod is left running —
// revocation is authority-only and reversible via resumeNode. Real-time denial
// comes from the resource's SpiceDB check; a short token lifetime bounds how long
// an already-minted token survives (client-credentials agents can still mint, but
// every call is re-checked against the dropped relation).
//
// It is a no-op (returns false) for any agent not currently "active", so a
// cascade never clobbers a descendant that already exited (completed/failed/
// killed) — that node keeps its terminal status and resumeNode won't resurrect
// it. This also makes revoke idempotent.
func revokeNode(ctx context.Context, s *store, sdb spicedb.Client, id string) bool {
	if s.getAgent(id).Status != "active" {
		return false
	}
	s.updateAgent(id, "revoked")
	if err := sdb.DeleteAgentRelationships(ctx, id); err != nil {
		log.Printf("spicedb cleanup error for %s: %v", id, err)
	}
	s.appendEvent(id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_revoked",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	})
	return true
}

// resumeNode reverses revokeNode: re-derive the agent's SpiceDB relations from
// its template (identical logic to registration) and mark it active again. It is
// a no-op (returns false) for any agent not currently "revoked", so resuming a
// subtree never resurrects a node that exited or was killed on its own.
func resumeNode(ctx context.Context, s *store, sdb spicedb.Client, id string) bool {
	rec := s.getAgent(id)
	if rec.Status != "revoked" {
		return false
	}
	tpl, ok := s.getTemplate(rec.AgentType)
	if !ok {
		log.Printf("resume: unknown agent type %q for %s", rec.AgentType, id)
		return false
	}
	s.updateAgent(id, "active")
	for _, rel := range tpl.AuthZ.SpiceDBRelations {
		// Global (tenant-less) agents must not produce a malformed "tenant:"
		// tuple, so skip any relation referencing {{tenant_id}}.
		if rec.TenantID == "" && referencesTenant(rel) {
			continue
		}
		res := substitute(rel.Resource, id, rec.TenantID)
		sub := substitute(rel.Subject, id, rec.TenantID)
		if err := sdb.WriteRelationship(ctx, res, rel.Relation, sub); err != nil {
			log.Printf("spicedb resume write error for %s: %v", id, err)
		}
	}
	s.appendEvent(id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_resumed",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	})
	return true
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

		// A restarting sidecar (native sidecars restart independently of the
		// pod) re-registers on boot. Never let that resurrect a record whose
		// SpiceDB authority was deliberately dropped — a failed/completed agent
		// stays terminal and a revoked one stays revoked until /resume.
		if existing := s.getAgent(agentID); existing.AgentID != "" {
			switch existing.Status {
			case "completed", "failed", "revoked":
				http.Error(w, fmt.Sprintf("agent %s is %s; re-registration refused", agentID, existing.Status),
					http.StatusConflict)
				return
			}
		}

		rec := registry.AgentRecord{
			AgentID:      agentID,
			AgentType:    req.AgentType,
			TenantID:     req.TenantID,
			UserID:       req.UserID,
			Status:       "active",
			Lifecycle:    tpl.Runtime.Lifecycle,
			SupportsChat: tpl.Runtime.SupportsChat,
			ParentID:     req.ParentID,
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
		tuples := []relTuple{}
		for _, rel := range tpl.AuthZ.SpiceDBRelations {
			// Global (tenant-less) agents must not produce a malformed
			// "tenant:" tuple, so skip any relation referencing {{tenant_id}}.
			if req.TenantID == "" && referencesTenant(rel) {
				continue
			}
			res := substitute(rel.Resource, agentID, req.TenantID)
			sub := substitute(rel.Subject, agentID, req.TenantID)
			if err := sdb.WriteRelationship(r.Context(), res, rel.Relation, sub); err != nil {
				log.Printf("spicedb write error: %v", err)
			}
			tuples = append(tuples, relTuple{
				Resource: res,
				Relation: rel.Relation,
				Subject:  sub,
			})
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

	// Revoke is the cascading revocation action: revoke the named agent AND its
	// entire descendant subtree (everything it spawned, transitively). For each
	// active node it drops the SpiceDB authorization and marks the record
	// "revoked", so permission checks deny in real time. Pods are left running —
	// revocation is authority-only and reversible via /resume. Ancestors and
	// siblings are untouched, as are descendants that already exited (their
	// terminal status is preserved). The response lists only the nodes actually
	// revoked. Revoking a leaf agent (no descendants) is the single-agent case.
	mux.HandleFunc("POST /v1/agents/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		subtree := s.subtree(r.PathValue("id"))
		if subtree == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		revoked := []string{}
		for _, id := range subtree {
			if revokeNode(r.Context(), s, sdb, id) {
				revoked = append(revoked, id)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"revoked": revoked})
	})

	// Resume reverses a revoke across the same subtree: for the named agent and
	// each descendant currently "revoked", re-derive its SpiceDB relations from
	// the template and mark it active. Nodes that aren't revoked (e.g.
	// completed/failed/killed) are skipped, so resume never resurrects an agent
	// that exited on its own. Idempotent: resuming an already-active subtree is a
	// no-op that returns an empty list.
	mux.HandleFunc("POST /v1/agents/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		subtree := s.subtree(r.PathValue("id"))
		if subtree == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resumed := []string{}
		for _, id := range subtree {
			if resumeNode(r.Context(), s, sdb, id) {
				resumed = append(resumed, id)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resumed": resumed})
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

	// Spawn-policy decision: may the agent identified by parentId spawn a child of
	// childType? Resolves parentId -> agentType -> template in-process and checks
	// the parent template's allowedChildTypes (deny-by-default: a parent whose
	// template lists no matching child types may spawn none) and its maxDepth (a
	// positive maxDepth caps total chain length, so the deepest agent cannot keep
	// spawning). The orchestrator calls this at spawn time when a parentId is
	// present.
	mux.HandleFunc("GET /v1/spawn-policy", func(w http.ResponseWriter, r *http.Request) {
		parentID := r.URL.Query().Get("parentId")
		childType := r.URL.Query().Get("childType")

		resp := registry.SpawnDecision{Allowed: false}

		rec := s.getAgent(parentID)
		switch {
		case rec.AgentID == "":
			resp.Reason = "unknown parent"
		default:
			tpl, ok := s.getTemplate(rec.AgentType)
			if !ok {
				resp.Reason = "parent template not found"
				break
			}
			for _, ct := range tpl.Delegation.AllowedChildTypes {
				if ct == childType {
					resp.Allowed = true
					break
				}
			}
			if !resp.Allowed {
				resp.Reason = fmt.Sprintf("%s may not spawn %s", rec.AgentType, childType)
				break
			}
			// Depth cap: the child would sit one level below the parent. A
			// positive maxDepth bounds total chain length (maxDepth == 0 means
			// unset / no limit, matching the delegation-policy default).
			if max := tpl.Delegation.MaxDepth; max > 0 {
				if childDepth := s.depth(parentID) + 1; childDepth > max {
					resp.Allowed = false
					resp.Reason = fmt.Sprintf("max delegation depth %d reached", max)
				}
			}
			// Surface the parent template's per-child consent gate so the
			// orchestrator can stamp it onto the workload.
			if resp.Allowed {
				if cp, ok := tpl.Delegation.ChildPolicies[childType]; ok && cp.RequireUserConsent {
					resp.ConsentRequired = true
				}
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	// Record a fresh user consent for one spawn edge (called by IdentityServer
	// when the user approves a CIBA request, or auto-renewed on auto-approval).
	// The expiry is derived here from the parent template's consentTTL so the
	// policy lives in one place. Replaces any prior record for the same edge.
	mux.HandleFunc("POST /v1/consents", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID     string   `json:"userId"`
			ParentType string   `json:"parentType"`
			ChildType  string   `json:"childType"`
			Scopes     []string `json:"scopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.UserID == "" || req.ParentType == "" || req.ChildType == "" {
			http.Error(w, "userId, parentType and childType are required", http.StatusBadRequest)
			return
		}
		if req.Scopes == nil {
			req.Scopes = []string{}
		}
		now := time.Now()
		rec := s.upsertConsent(registry.ConsentRecord{
			UserID:     req.UserID,
			ParentType: req.ParentType,
			ChildType:  req.ChildType,
			Scopes:     req.Scopes,
			GrantedAt:  now,
			ExpiresAt:  s.consentExpiry(req.ParentType, req.ChildType, now),
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(rec)
	})

	mux.HandleFunc("GET /v1/consents", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.listConsents(r.URL.Query().Get("userId")))
	})

	// Revoke a stored consent: the next spawn of that edge re-prompts the user.
	// Live agents are untouched — cascading a revoke of agents spawned under
	// this consent is a separate, explicit /v1/agents/{id}/revoke call. An
	// optional userId query scopes the revoke to that user's own records
	// (a mismatch is indistinguishable from an unknown id).
	mux.HandleFunc("POST /v1/consents/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		if !s.revokeConsent(r.PathValue("id"), r.URL.Query().Get("userId")) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Consent decision for a spawn edge: does a stored consent cover the
	// requested scopes right now? Always HTTP 200; IdentityServer's CIBA
	// notification hook calls this to decide auto-approve vs. prompt-the-user.
	// Scopes repeat: ?scope=a&scope=b.
	mux.HandleFunc("GET /v1/consents/check", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		decision := registry.ConsentDecision{Granted: false, Reason: "no consent on record"}
		if rec, ok := s.findConsent(q.Get("userId"), q.Get("parentType"), q.Get("childType")); ok {
			decision = registry.EvaluateConsent(rec, q["scope"], time.Now())
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(decision)
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
