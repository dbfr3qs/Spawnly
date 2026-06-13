// cmd/registry/main.go
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

// defaultSchema is the platform's built-in SpiceDB schema, written to SpiceDB
// on boot and used to validate templates. A consumer can override it at runtime
// via SPICEDB_SCHEMA_PATH (see loadActiveSchema) without rebuilding. This file
// is the single source of truth; deploy/spicedb/schema.zed is a reference copy.
//
//go:embed schema.zed
var defaultSchema string

const defaultSchemaVersion = "v1"

type store struct {
	mu        sync.RWMutex
	templates map[string]registry.AgentTemplate
	agents    map[string]registry.AgentRecord
	events    map[string][]events.Event
	// consents is keyed by the (user, parentType, childType) edge — one record
	// per edge, replaced on each fresh grant. See consentKey.
	consents map[string]registry.ConsentRecord

	// consentRequests holds brokered consent asks by ID (Phase 5b). openRequests
	// indexes the single open (pending) request per edge by consentKey, so
	// creating a request while one is pending returns the existing one rather
	// than firing a duplicate prompt.
	consentRequests map[string]registry.ConsentRequest
	openRequests    map[string]string // consentKey -> pending request ID

	// Active SpiceDB schema: the text written to SpiceDB on boot, the model used
	// to validate templates, plus version/source for GET /v1/schema. Seeded with
	// the embedded default in newStore; main() overrides it from
	// SPICEDB_SCHEMA_PATH when set.
	schemaModel   *registry.SchemaModel
	schemaText    string
	schemaVersion string
	schemaSource  string
}

func newStore() *store {
	s := &store{
		templates:       map[string]registry.AgentTemplate{},
		agents:          map[string]registry.AgentRecord{},
		events:          map[string][]events.Event{},
		consents:        map[string]registry.ConsentRecord{},
		consentRequests: map[string]registry.ConsentRequest{},
		openRequests:    map[string]string{},
	}
	s.setSchema(defaultSchema, defaultSchemaVersion, "default")
	return s
}

// setSchema records the active schema text/version/source and parses it into a
// validation model. A parse failure disables template validation (model stays
// nil) rather than crashing — the embedded default is known-good, so this only
// degrades gracefully for a malformed override.
func (s *store) setSchema(text, version, source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schemaText, s.schemaVersion, s.schemaSource = text, version, source
	m, err := registry.ParseSchema(text)
	if err != nil {
		log.Printf("schema parse failed (source=%s); template validation disabled: %v", source, err)
		s.schemaModel = nil
		return
	}
	s.schemaModel = m
}

// validateTemplate checks a template's relations against the active schema.
// Returns nil when validation is disabled (no parsed model).
func (s *store) validateTemplate(t registry.AgentTemplate) error {
	s.mu.RLock()
	m := s.schemaModel
	s.mu.RUnlock()
	if m == nil {
		return nil
	}
	return m.Validate(t.AuthZ)
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
	return s.upsertConsentLocked(rec)
}

// upsertConsentLocked is upsertConsent's body; callers must already hold s.mu.
// It lets the consent-request resolve path upsert a grant while holding the
// write lock without re-entering the (non-reentrant) mutex.
func (s *store) upsertConsentLocked(rec registry.ConsentRecord) registry.ConsentRecord {
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

// createConsentRequest returns the open pending request for the edge if one
// already exists (created=false — idempotent re-notify, not a duplicate
// prompt), otherwise stores and returns a fresh pending request (created=true).
func (s *store) createConsentRequest(req registry.ConsentRequest) (registry.ConsentRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := consentKey(req.UserID, req.ParentType, req.ChildType)
	if id, ok := s.openRequests[key]; ok {
		if existing, ok := s.consentRequests[id]; ok && existing.Status == registry.ConsentPending {
			return existing, false
		}
	}
	if req.Scopes == nil {
		req.Scopes = []string{}
	}
	req.ID = fmt.Sprintf("creq-%d", time.Now().UnixNano())
	req.Status = registry.ConsentPending
	req.CreatedAt = time.Now()
	s.consentRequests[req.ID] = req
	s.openRequests[key] = req.ID
	return req, true
}

// createApprovedConsentRequest stores a request already resolved as approved —
// used when a covering ConsentRecord already exists, so no prompt is needed.
func (s *store) createApprovedConsentRequest(req registry.ConsentRequest) registry.ConsentRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if req.Scopes == nil {
		req.Scopes = []string{}
	}
	req.ID = fmt.Sprintf("creq-%d", now.UnixNano())
	req.Status = registry.ConsentApproved
	req.CreatedAt = now
	req.ResolvedAt = &now
	s.consentRequests[req.ID] = req
	return req
}

func (s *store) getConsentRequest(id string) (registry.ConsentRequest, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cr, ok := s.consentRequests[id]
	return cr, ok
}

// listConsentRequests returns brokered requests, optionally filtered by userID
// and/or status (e.g. "pending"). Empty filters match all.
func (s *store) listConsentRequests(userID, status string) []registry.ConsentRequest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []registry.ConsentRequest{}
	for _, cr := range s.consentRequests {
		if userID != "" && cr.UserID != userID {
			continue
		}
		if status != "" && string(cr.Status) != status {
			continue
		}
		out = append(out, cr)
	}
	return out
}

// resolveConsentRequest approves or denies a pending request. On approve it
// upserts a ConsentRecord (identical to POST /v1/consents) and sweeps any other
// pending request for the same edge that the fresh grant now covers — making
// the registry, not the IdP, the owner of the full consent lifecycle. It is
// idempotent: resolving an already-resolved request returns it unchanged.
//
// A non-empty userID scopes the resolve to that user's own request (the
// dashboard passes the session user, so one user cannot approve another's
// pending request — a mismatch is indistinguishable from an unknown id). The
// CIBA driver passes "" because it has already authenticated the user.
func (s *store) resolveConsentRequest(id, userID string, approve bool, scopes []string) (registry.ConsentRequest, bool) {
	cr, ok := s.getConsentRequest(id)
	if !ok || (userID != "" && cr.UserID != userID) {
		return registry.ConsentRequest{}, false
	}

	now := time.Now()
	grantScopes := scopes
	var expiry *time.Time
	if approve {
		if grantScopes == nil {
			grantScopes = cr.Scopes
		}
		// Compute expiry before taking the write lock (consentExpiry RLocks).
		expiry = s.consentExpiry(cr.ParentType, cr.ChildType, now)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok = s.consentRequests[id]
	if !ok {
		return registry.ConsentRequest{}, false
	}
	if cr.Status != registry.ConsentPending {
		return cr, true // idempotent
	}
	cr.ResolvedAt = &now
	key := consentKey(cr.UserID, cr.ParentType, cr.ChildType)

	if !approve {
		cr.Status = registry.ConsentDenied
		s.consentRequests[id] = cr
		delete(s.openRequests, key)
		return cr, true
	}

	cr.Status = registry.ConsentApproved
	s.consentRequests[id] = cr
	delete(s.openRequests, key)

	granted := s.upsertConsentLocked(registry.ConsentRecord{
		UserID: cr.UserID, ParentType: cr.ParentType, ChildType: cr.ChildType,
		Scopes: grantScopes, GrantedAt: now, ExpiresAt: expiry,
	})

	// Sweep: any other still-pending request for the same edge now covered by
	// the grant is auto-approved too (mirrors CIBA's ResolvePendingForEdge).
	for rid, other := range s.consentRequests {
		if rid == id || other.Status != registry.ConsentPending {
			continue
		}
		if consentKey(other.UserID, other.ParentType, other.ChildType) != key {
			continue
		}
		if registry.EvaluateConsent(granted, other.Scopes, now).Granted {
			other.Status = registry.ConsentApproved
			other.ResolvedAt = &now
			s.consentRequests[rid] = other
		}
	}
	return cr, true
}

// subtree returns the agent id plus every descendant reachable through ParentID
// edges (everything the agent spawned, transitively), root first followed by a
// breadth-first walk. It is the set a cascading revoke/resume operates on.
// Returns nil if the id is unknown.
//
// It is built on the registry.Store primitives GetAgent + ListChildren (Phase
// 4), so the traversal logic is storage-agnostic: each BFS step is one backend
// call, which a DynamoDB Store turns into a GetItem / ParentID-GSI Query.
func subtree(ctx context.Context, s registry.Store, id string) []string {
	root, _ := s.GetAgent(ctx, id)
	if root.AgentID == "" {
		return nil
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
		children, _ := s.ListChildren(ctx, cur)
		for _, c := range children {
			queue = append(queue, c.AgentID)
		}
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

// relationResourceTypes derives the de-duplicated set of SpiceDB resource types
// a template's relations write to — the type prefix of each rel.Resource (the
// static text before ':', never a placeholder). This is exactly the set that
// must be passed to DeleteAgentRelationships to clean an agent up, replacing the
// old hardcoded "tenant" assumption so any consumer schema works. When hasTenant
// is false, relations referencing {{tenant_id}} are skipped — they were never
// written for a global agent (same rule as registration/resume).
func relationResourceTypes(tpl registry.AgentTemplate, hasTenant bool) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, rel := range tpl.AuthZ.SpiceDBRelations {
		if !hasTenant && referencesTenant(rel) {
			continue
		}
		resType := strings.SplitN(rel.Resource, ":", 2)[0]
		if resType == "" || seen[resType] {
			continue
		}
		seen[resType] = true
		out = append(out, resType)
	}
	return out
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
	// Phase 5a: revoke is reversible, so drop only the agent's `enabled` status
	// tuple — a single write regardless of template size. The template relations
	// are deliberately left in place so resumeNode can re-enable in O(1) without
	// re-deriving them from a template that may have since changed or been
	// deleted. Permission denial is immediate: `work_on = agent & agent->enabled`
	// fails the intersection the moment this tuple is gone.
	if err := sdb.DeleteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id); err != nil {
		log.Printf("spicedb revoke (enabled-tuple delete) error for %s: %v", id, err)
	}
	s.appendEvent(id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_revoked",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	})
	return true
}

// resumeNode reverses revokeNode by re-writing the agent's single `enabled`
// status tuple and marking it active again (Phase 5a). It is a no-op (returns
// false) for any agent not currently "revoked", so resuming a subtree never
// resurrects a node that exited or was killed on its own.
//
// Resume no longer re-derives relations from the template: the template
// relations were never removed by revoke, so re-enabling is a single write that
// does not depend on the template still existing or matching what it was at
// registration time (fixing the re-derivation-drift risk of the old approach).
func resumeNode(ctx context.Context, s *store, sdb spicedb.Client, id string) bool {
	if s.getAgent(id).Status != "revoked" {
		return false
	}
	s.updateAgent(id, "active")
	if err := sdb.WriteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id); err != nil {
		log.Printf("spicedb resume (enabled-tuple write) error for %s: %v", id, err)
	}
	s.appendEvent(id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_resumed",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	})
	return true
}

func buildMux(s *store, sdb spicedb.Client, verifier registrant.Verifier) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		var t registry.AgentTemplate
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Reject a template whose relations don't conform to the active schema
		// before storing it, so a mismatch is caught at registration time rather
		// than silently failing every tuple write at agent-register time.
		if err := s.validateTemplate(t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.putTemplate(t)
		w.WriteHeader(http.StatusCreated)
	})

	// Returns the active SpiceDB schema the registry applied on boot, its
	// version, and whether it's the embedded default or an override. Public
	// (no SVID) — it's the contract a consumer validates their templates against.
	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		resp := map[string]string{
			"schema":  s.schemaText,
			"version": s.schemaVersion,
			"source":  s.schemaSource,
		}
		s.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
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
		identity, err := verifier.Verify(r.Context(), r)
		if err != nil {
			log.Printf("registration auth failed: %v", err)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		agentID := identity.AgentID
		if agentID == "" {
			http.Error(w, "registrant verifier returned empty agentID", http.StatusInternalServerError)
			return
		}
		log.Printf("registering agent %s (issuer=%s subject=%s)", agentID, identity.Issuer, identity.Subject)

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
		// Write the agent's `enabled` status tuple (Phase 5a). The default
		// schema gates every revocable permission on `& agent->enabled`, so this
		// single tuple is what revoke/resume toggle — the template relations
		// above are left untouched by a revoke.
		enabledRes, enabledSub := "agent:"+agentID, "agent:"+agentID
		if err := sdb.WriteRelationship(r.Context(), enabledRes, "enabled", enabledSub); err != nil {
			log.Printf("spicedb enabled-tuple write error for %s: %v", agentID, err)
		}
		tuples = append(tuples, relTuple{Resource: enabledRes, Relation: "enabled", Subject: enabledSub})
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
			// Terminal agents never resume, so fully clean up: delete the
			// template relations (resource types derived per Phase 1) and the
			// agent's `enabled` status tuple. This is the irreversible
			// counterpart to revoke, which only toggles `enabled`.
			rec := s.getAgent(agentID)
			var resTypes []string
			if tpl, ok := s.getTemplate(rec.AgentType); ok {
				resTypes = relationResourceTypes(tpl, rec.TenantID != "")
			}
			if err := sdb.DeleteAgentRelationships(r.Context(), agentID, resTypes); err != nil {
				log.Printf("spicedb cleanup error for %s: %v", agentID, err)
			}
			if err := sdb.DeleteRelationship(r.Context(), "agent:"+agentID, "enabled", "agent:"+agentID); err != nil {
				log.Printf("spicedb enabled-tuple cleanup error for %s: %v", agentID, err)
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
		nodes := subtree(r.Context(), s, r.PathValue("id"))
		if nodes == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		revoked := []string{}
		for _, id := range nodes {
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
		nodes := subtree(r.Context(), s, r.PathValue("id"))
		if nodes == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resumed := []string{}
		for _, id := range nodes {
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

	// --- Registry-native consent broker (Phase 5b) ---------------------------
	// The registry owns the full pending->approved/denied consent lifecycle, so
	// consent no longer requires a CIBA-capable IdP. CIBA becomes one optional
	// driver that creates/resolves requests through these endpoints; a non-CIBA
	// consumer drives them directly (e.g. from a dashboard approve/deny UI).

	// Create (or return the existing open) pending consent request for an edge.
	// Short-circuits to "approved" with no prompt/webhook when a stored consent
	// already covers the requested scopes. Fires the notifier webhook otherwise.
	mux.HandleFunc("POST /v1/consent-requests", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID         string   `json:"userId"`
			ParentType     string   `json:"parentType"`
			ChildType      string   `json:"childType"`
			Scopes         []string `json:"scopes"`
			BindingMessage string   `json:"bindingMessage"`
			ExternalRef    string   `json:"externalRef"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.UserID == "" || req.ParentType == "" || req.ChildType == "" {
			http.Error(w, "userId, parentType and childType are required", http.StatusBadRequest)
			return
		}
		cr := registry.ConsentRequest{
			UserID: req.UserID, ParentType: req.ParentType, ChildType: req.ChildType,
			Scopes: req.Scopes, BindingMessage: req.BindingMessage, ExternalRef: req.ExternalRef,
		}

		// Short-circuit: an existing covering consent means no human ask needed.
		if rec, ok := s.findConsent(req.UserID, req.ParentType, req.ChildType); ok {
			if registry.EvaluateConsent(rec, req.Scopes, time.Now()).Granted {
				out := s.createApprovedConsentRequest(cr)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				json.NewEncoder(w).Encode(out)
				return
			}
		}

		out, created := s.createConsentRequest(cr)
		if created {
			go notifyWebhook(out)
		}
		w.Header().Set("Content-Type", "application/json")
		if created {
			w.WriteHeader(http.StatusCreated)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		json.NewEncoder(w).Encode(out)
	})

	mux.HandleFunc("GET /v1/consent-requests", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.listConsentRequests(q.Get("userId"), q.Get("status")))
	})

	mux.HandleFunc("GET /v1/consent-requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		cr, ok := s.getConsentRequest(r.PathValue("id"))
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cr)
	})

	mux.HandleFunc("POST /v1/consent-requests/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scopes []string `json:"scopes"`
		}
		// Body is optional — an empty/absent body approves the originally
		// requested scopes.
		_ = json.NewDecoder(r.Body).Decode(&body)
		cr, ok := s.resolveConsentRequest(r.PathValue("id"), r.URL.Query().Get("userId"), true, body.Scopes)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cr)
	})

	mux.HandleFunc("POST /v1/consent-requests/{id}/deny", func(w http.ResponseWriter, r *http.Request) {
		cr, ok := s.resolveConsentRequest(r.PathValue("id"), r.URL.Query().Get("userId"), false, nil)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cr)
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

// notifyWebhook posts a "consent_pending" notification for a freshly-created
// consent request to NOTIFIER_WEBHOOK_URL, if configured. Best-effort:
// log-and-continue on any failure (matching the prior IdentityServer behavior
// this replaces). The payload shape is unchanged so existing notifiers keep
// working after the webhook moved from the IdP into the registry.
func notifyWebhook(cr registry.ConsentRequest) {
	url := os.Getenv("NOTIFIER_WEBHOOK_URL")
	if url == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"type":           "consent_pending",
		"user":           cr.UserID,
		"parentType":     cr.ParentType,
		"childType":      cr.ChildType,
		"scopes":         cr.Scopes,
		"bindingMessage": cr.BindingMessage,
	})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("consent notifier webhook failed: %v", err)
		return
	}
	resp.Body.Close()
}

// loadActiveSchema returns the schema text, version, and source. When
// SPICEDB_SCHEMA_PATH is set its file contents become the active schema
// (source "override"); otherwise the embedded default is used. An unreadable
// override path is fatal — a consumer who set it clearly intends an override,
// and silently falling back would re-create the "looks fine, is broken" gap.
func loadActiveSchema() (schema, version, source string) {
	if p := os.Getenv("SPICEDB_SCHEMA_PATH"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			log.Fatalf("SPICEDB_SCHEMA_PATH=%s: %v", p, err)
		}
		return string(b), "custom", "override"
	}
	return defaultSchema, defaultSchemaVersion, "default"
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

	// The registry is the single writer of the SpiceDB schema (Phase 2). Apply
	// the active schema on boot before serving, retrying since SpiceDB may not
	// be ready immediately on first start.
	activeSchema, schemaVersion, schemaSource := loadActiveSchema()
	for i := 1; i <= 10; i++ {
		if err := sdb.WriteSchema(ctx, activeSchema); err == nil {
			break
		} else if i == 10 {
			log.Fatalf("WriteSchema failed after 10 attempts: %v", err)
		} else {
			log.Printf("WriteSchema attempt %d/10 failed, retrying: %v", i, err)
			time.Sleep(3 * time.Second)
		}
	}

	// Registration auth is pluggable (Phase 3): the verifier identifies the
	// caller registering an agent. Default is SPIFFE-SVID (SPIRE); consumers
	// without SPIRE can select OIDC-JWT or mTLS via REGISTRANT_VERIFIER.
	var verifier registrant.Verifier
	switch v := getEnv("REGISTRANT_VERIFIER", "spiffe-svid"); v {
	case "spiffe-svid":
		jwksValidator, err := spiffe.NewJWKSValidator(ctx, spireJWKSURL)
		if err != nil {
			log.Fatalf("SVID validator init: %v", err)
		}
		verifier = registrant.NewSpiffeVerifier(jwksValidator)
	case "oidc":
		cfg := registrant.OIDCConfig{
			JWKSURL:               getEnv("OIDC_JWKS_URL", ""),
			Audience:              getEnv("OIDC_AUDIENCE", "registry"),
			AgentIDClaim:          getEnv("OIDC_AGENT_ID_CLAIM", "sub"),
			InsecureSkipTLSVerify: getEnv("OIDC_INSECURE_SKIP_TLS_VERIFY", "false") == "true",
		}
		if cfg.JWKSURL == "" {
			log.Fatalf("OIDC_JWKS_URL required when REGISTRANT_VERIFIER=oidc")
		}
		verifier, err = registrant.NewOIDCVerifier(ctx, cfg)
		if err != nil {
			log.Fatalf("OIDC verifier init: %v", err)
		}
	case "mtls":
		if getEnv("MTLS_CLIENT_CA_PATH", "") == "" {
			log.Fatalf("MTLS_CLIENT_CA_PATH required when REGISTRANT_VERIFIER=mtls")
		}
		verifier = registrant.NewMTLSVerifier(registrant.MTLSConfig{
			AgentIDSource: getEnv("MTLS_AGENT_ID_SOURCE", "san_uri"),
		})
		// Note: actual mTLS termination wiring (ListenAndServeTLS + ClientCAs)
		// is a separate change to the http.Server setup below — out of scope
		// for this phase's interface work; this case documents the config
		// contract and fails fast if misconfigured.
	default:
		log.Fatalf("unknown REGISTRANT_VERIFIER %q", v)
	}

	s := newStore()
	s.setSchema(activeSchema, schemaVersion, schemaSource)
	log.Printf("registry listening on :8080 (schema source=%s version=%s)", schemaSource, schemaVersion)
	log.Fatal(http.ListenAndServe(":8080", buildMux(s, sdb, verifier)))
}
