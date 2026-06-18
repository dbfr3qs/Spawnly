// cmd/registry/main.go
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spawnly/platform/internal/controlplane"
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

// validAgentID constrains the identifier a registrant.Verifier returns before it
// is used to build SpiceDB tuple keys and as the AgentRecord primary key. The
// allowed set covers SPIFFE-derived ids (e.g. "chain-worker-ab12cd") while
// rejecting ':' / '/' / whitespace that would corrupt "agent:"+id tuples.
var validAgentID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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

// updateTemplateStatus sets Status on an existing template, returning the
// updated template and whether it was found.
func (s *store) updateTemplateStatus(agentType, status string) (registry.AgentTemplate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.templates[agentType]
	if !ok {
		return registry.AgentTemplate{}, false
	}
	t.Status = status
	s.templates[agentType] = t
	return t, true
}

// deleteTemplate removes a template, returning whether one was found.
func (s *store) deleteTemplate(agentType string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.templates[agentType]; !ok {
		return false
	}
	delete(s.templates, agentType)
	return true
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

// appendEvent stamps and stores an event, returning the stored copy. It returns
// the value it wrote rather than having callers re-read the tail of the list,
// so the AppendEvent contract ("returns the stored event") holds without a
// second locked read — and so a durable backend can return what it persisted
// instead of relying on an in-process append order.
func (s *store) appendEvent(agentID string, e events.Event) events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	e.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	e.Timestamp = time.Now()
	s.events[agentID] = append(s.events[agentID], e)
	return e
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
	key := openRequestKey(req.UserID, req.ParentType, req.ChildType, req.AgentID)
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

// openRequestKey is the dedup key for an open (pending) consent request. It is
// the specific waiting agent when known (so each consent-gated agent gets its
// own correlatable request, and a retry from the same agent's sidecar dedupes),
// falling back to the (user, parentType, childType) edge for callers that don't
// identify an agent. Consent *grants* and the approve-sweep remain per-edge.
func openRequestKey(userID, parentType, childType, agentID string) string {
	if agentID != "" {
		return "agent:" + agentID
	}
	return consentKey(userID, parentType, childType)
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
		delete(s.openRequests, openRequestKey(cr.UserID, cr.ParentType, cr.ChildType, cr.AgentID))
		return cr, true
	}

	cr.Status = registry.ConsentApproved
	s.consentRequests[id] = cr
	delete(s.openRequests, openRequestKey(cr.UserID, cr.ParentType, cr.ChildType, cr.AgentID))

	granted := s.upsertConsentLocked(registry.ConsentRecord{
		UserID: cr.UserID, ParentType: cr.ParentType, ChildType: cr.ChildType,
		Scopes: grantScopes, GrantedAt: now, ExpiresAt: expiry,
	})

	// Sweep: any other still-pending request for the same edge now covered by
	// the grant is auto-approved too — so every other agent waiting on this same
	// (user, parentType, childType) edge activates without its own prompt.
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
			delete(s.openRequests, openRequestKey(other.UserID, other.ParentType, other.ChildType, other.AgentID))
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
func depth(ctx context.Context, s registry.Store, id string) int {
	d := 0
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		rec, _ := s.GetAgent(ctx, cur)
		if rec.AgentID == "" {
			break
		}
		seen[cur] = true // cycle guard
		d++
		cur = rec.ParentID
	}
	return d
}

// storeErr writes a 500 for a Store error and reports whether it did, so a
// handler can `if storeErr(w, err) { return }`. The in-memory store never
// errors; this is the path a durable backend (e.g. DynamoDB) uses.
func storeErr(w http.ResponseWriter, err error) bool {
	if err != nil {
		log.Printf("store error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return true
	}
	return false
}

// validAgentType guards the {type} segment of the template write routes, which
// is extracted by trimming the path prefix. It rejects an empty segment (a bare
// /v1/templates/ request) or one containing a slash (a nested path) with a 400,
// so those land as an intentional bad request rather than an accidental 404.
func validAgentType(w http.ResponseWriter, agentType string) bool {
	if agentType == "" || strings.Contains(agentType, "/") {
		http.Error(w, "invalid agent type", http.StatusBadRequest)
		return false
	}
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
func revokeNode(ctx context.Context, s registry.Store, sdb spicedb.Client, id string) bool {
	// Best-effort/idempotent: store errors are logged-and-ignored (the in-memory
	// store never errors; a durable backend's transient error must not abort a
	// cascade mid-subtree).
	if rec, _ := s.GetAgent(ctx, id); rec.Status != "active" {
		return false
	}
	s.UpdateAgentStatus(ctx, id, "revoked")
	// Phase 5a: revoke is reversible, so drop only the agent's `enabled` status
	// tuple — a single write regardless of template size. The template relations
	// are deliberately left in place so resumeNode can re-enable in O(1) without
	// re-deriving them from a template that may have since changed or been
	// deleted. Permission denial is immediate: `work_on = agent & agent->enabled`
	// fails the intersection the moment this tuple is gone.
	if err := sdb.DeleteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id); err != nil {
		log.Printf("spicedb revoke (enabled-tuple delete) error for %s: %v", id, err)
	}
	s.AppendEvent(ctx, id, events.Event{
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
func resumeNode(ctx context.Context, s registry.Store, sdb spicedb.Client, id string) bool {
	if rec, _ := s.GetAgent(ctx, id); rec.Status != "revoked" {
		return false
	}
	s.UpdateAgentStatus(ctx, id, "active")
	if err := sdb.WriteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id); err != nil {
		log.Printf("spicedb resume (enabled-tuple write) error for %s: %v", id, err)
	}
	s.AppendEvent(ctx, id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_resumed",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	})
	return true
}

func buildMux(s registry.Store, sdb spicedb.Client, verifier registrant.Verifier, cpAuth controlplane.Authenticator) *http.ServeMux {
	mux := http.NewServeMux()

	// Control-plane caller authentication for the consent lifecycle endpoints.
	// These are invoked only by trusted control-plane services (the orchestrator
	// proxying the dashboard, and the IdP's CIBA driver), never by agents — so
	// they sit behind the pluggable control-plane Authenticator rather than the
	// agent registrant verifier. The default (AllowAll) runs them open for the
	// local demo; a shared-secret or OIDC authenticator gates them in
	// production. The per-user confused-deputy scoping (the userId param,
	// asserted by the authenticated dashboard) is unchanged and layers on top.
	cp := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if _, err := cpAuth.Authenticate(r.Context(), r); err != nil {
				log.Printf("control-plane auth failed: %v", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	// handleCP registers a consent-lifecycle handler behind the control-plane check.
	handleCP := func(pattern string, h http.HandlerFunc) { mux.HandleFunc(pattern, cp(h)) }

	// Template management (create/disable/delete) is a control-plane operation,
	// so it sits behind the same control-plane authenticator as the consent
	// lifecycle. Under the default AllowAll authenticator it runs open.
	mux.HandleFunc("POST /v1/templates", cp(func(w http.ResponseWriter, r *http.Request) {
		var t registry.AgentTemplate
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Reject an unrecognized status at creation time so a template can't be
		// born with a junk value. Empty (treated as active) and the tolerated
		// legacy "deprecated" are both allowed; PATCH is stricter (active/disabled).
		switch t.Status {
		case "", registry.TemplateStatusActive, registry.TemplateStatusDisabled, "deprecated":
		default:
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		// Reject a template whose relations don't conform to the active schema
		// before storing it, so a mismatch is caught at registration time rather
		// than silently failing every tuple write at agent-register time.
		if err := s.ValidateTemplate(t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if storeErr(w, s.PutTemplate(r.Context(), t)) {
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))

	// PATCH /v1/templates/{type} — set a template's status. Only "active" and
	// "disabled" are accepted; "disabled" hides the template from the spawnable
	// list and (in a separate orchestrator step) blocks new spawns.
	mux.HandleFunc("PATCH /v1/templates/", cp(func(w http.ResponseWriter, r *http.Request) {
		agentType := strings.TrimPrefix(r.URL.Path, "/v1/templates/")
		if !validAgentType(w, agentType) {
			return
		}
		var body struct {
			Status string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if body.Status != registry.TemplateStatusActive && body.Status != registry.TemplateStatusDisabled {
			http.Error(w, "invalid status", http.StatusBadRequest)
			return
		}
		t, found, err := s.UpdateTemplateStatus(r.Context(), agentType, body.Status)
		if storeErr(w, err) {
			return
		}
		if !found {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(t)
	}))

	// DELETE /v1/templates/{type} — remove a template, but only once it has been
	// disabled, so a spawnable template can't be deleted out from under live spawns.
	mux.HandleFunc("DELETE /v1/templates/", cp(func(w http.ResponseWriter, r *http.Request) {
		agentType := strings.TrimPrefix(r.URL.Path, "/v1/templates/")
		if !validAgentType(w, agentType) {
			return
		}
		t, ok, err := s.GetTemplate(r.Context(), agentType)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if t.Status != registry.TemplateStatusDisabled {
			http.Error(w, "template must be disabled before deletion", http.StatusConflict)
			return
		}
		if _, err := s.DeleteTemplate(r.Context(), agentType); storeErr(w, err) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	// Returns the active SpiceDB schema the registry applied on boot, its
	// version, and whether it's the embedded default or an override. Public
	// (no SVID) — it's the contract a consumer validates their templates against.
	mux.HandleFunc("GET /v1/schema", func(w http.ResponseWriter, r *http.Request) {
		text, version, source := s.Schema()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"schema": text, "version": version, "source": source})
	})

	mux.HandleFunc("GET /v1/templates/", func(w http.ResponseWriter, r *http.Request) {
		agentType := strings.TrimPrefix(r.URL.Path, "/v1/templates/")
		t, ok, err := s.GetTemplate(r.Context(), agentType)
		if storeErr(w, err) {
			return
		}
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
		if storeErr(w, s.RegisterAgent(r.Context(), rec)) {
			return
		}
		s.AppendEvent(r.Context(), rec.AgentID, events.Event{
			Source:  events.SourceOrchestrator,
			Type:    "workload_spawning",
			Payload: mustMarshal(map[string]string{"agentId": rec.AgentID, "agentType": rec.AgentType}),
		})
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("POST /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		identity, err := verifier.Verify(ctx, r)
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
		// The agentID becomes a SpiceDB object id ("agent:"+id) and the
		// AgentRecord primary key. SPIFFE ids (path.Base of the SVID) already
		// conform; this guards the OIDC/mTLS paths, where the id comes from a
		// consumer-controlled claim or cert SAN — an unconstrained value (':',
		// '/', whitespace) would corrupt every tuple key written for the agent.
		if !validAgentID.MatchString(agentID) {
			log.Printf("rejecting registration: invalid agentID %q (issuer=%s)", agentID, identity.Issuer)
			http.Error(w, "invalid agent id", http.StatusBadRequest)
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

		tpl, ok, err := s.GetTemplate(ctx, req.AgentType)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "unknown agent type", http.StatusBadRequest)
			return
		}

		// A restarting sidecar (native sidecars restart independently of the
		// pod) re-registers on boot. Never let that resurrect a record whose
		// SpiceDB authority was deliberately dropped — a failed/completed agent
		// stays terminal and a revoked one stays revoked until /resume.
		existing, err := s.GetAgent(ctx, agentID)
		if storeErr(w, err) {
			return
		}
		if existing.AgentID != "" {
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
		if storeErr(w, s.RegisterAgent(ctx, rec)) {
			return
		}
		s.AppendEvent(ctx, agentID, events.Event{
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
			if err := sdb.WriteRelationship(ctx, res, rel.Relation, sub); err != nil {
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
		if err := sdb.WriteRelationship(ctx, enabledRes, "enabled", enabledSub); err != nil {
			log.Printf("spicedb enabled-tuple write error for %s: %v", agentID, err)
		}
		tuples = append(tuples, relTuple{Resource: enabledRes, Relation: "enabled", Subject: enabledSub})
		s.AppendEvent(ctx, agentID, events.Event{
			Source:  events.SourceRegistry,
			Type:    "spicedb_relations_written",
			Payload: mustMarshal(tuples),
		})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(rec)
	})

	mux.HandleFunc("GET /v1/agents", func(w http.ResponseWriter, r *http.Request) {
		agents, err := s.ListAgents(r.Context())
		if storeErr(w, err) {
			return
		}
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
		stored, err := s.AppendEvent(r.Context(), agentID, e)
		if storeErr(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(stored)
	})

	mux.HandleFunc("GET /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		evts, err := s.GetEvents(r.Context(), agentID)
		if storeErr(w, err) {
			return
		}
		if evts == nil {
			evts = []events.Event{}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(evts)
	})

	mux.HandleFunc("GET /v1/agents/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		rec, err := s.GetAgent(r.Context(), agentID)
		if storeErr(w, err) {
			return
		}
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
		ctx := r.Context()
		ok, err := s.UpdateAgentStatus(ctx, agentID, req.Status)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if req.Status == "completed" || req.Status == "failed" {
			// Terminal agents never resume, so fully clean up: delete the
			// template relations (resource types derived per Phase 1) and the
			// agent's `enabled` status tuple. This is the irreversible
			// counterpart to revoke, which only toggles `enabled`.
			rec, _ := s.GetAgent(ctx, agentID)
			var resTypes []string
			if tpl, ok, _ := s.GetTemplate(ctx, rec.AgentType); ok {
				resTypes = relationResourceTypes(tpl, rec.TenantID != "")
			}
			if err := sdb.DeleteAgentRelationships(ctx, agentID, resTypes); err != nil {
				log.Printf("spicedb cleanup error for %s: %v", agentID, err)
			}
			if err := sdb.DeleteRelationship(ctx, "agent:"+agentID, "enabled", "agent:"+agentID); err != nil {
				log.Printf("spicedb enabled-tuple cleanup error for %s: %v", agentID, err)
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		ok, err := s.DismissAgent(r.Context(), agentID)
		if storeErr(w, err) {
			return
		}
		if !ok {
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

		tpl, ok, err := s.GetTemplate(r.Context(), parentType)
		if storeErr(w, err) {
			return
		}
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
		ctx := r.Context()

		rec, err := s.GetAgent(ctx, parentID)
		if storeErr(w, err) {
			return
		}
		switch {
		case rec.AgentID == "":
			resp.Reason = "unknown parent"
		default:
			tpl, ok, err := s.GetTemplate(ctx, rec.AgentType)
			if storeErr(w, err) {
				return
			}
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
				if childDepth := depth(ctx, s, parentID) + 1; childDepth > max {
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
	handleCP("POST /v1/consents", func(w http.ResponseWriter, r *http.Request) {
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
		ctx := r.Context()
		now := time.Now()
		expiry, err := s.ConsentExpiry(ctx, req.ParentType, req.ChildType, now)
		if storeErr(w, err) {
			return
		}
		rec, err := s.UpsertConsent(ctx, registry.ConsentRecord{
			UserID:     req.UserID,
			ParentType: req.ParentType,
			ChildType:  req.ChildType,
			Scopes:     req.Scopes,
			GrantedAt:  now,
			ExpiresAt:  expiry,
		})
		if storeErr(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(rec)
	})

	handleCP("GET /v1/consents", func(w http.ResponseWriter, r *http.Request) {
		consents, err := s.ListConsents(r.Context(), r.URL.Query().Get("userId"))
		if storeErr(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(consents)
	})

	// Revoke a stored consent: the next spawn of that edge re-prompts the user.
	// Live agents are untouched — cascading a revoke of agents spawned under
	// this consent is a separate, explicit /v1/agents/{id}/revoke call. An
	// optional userId query scopes the revoke to that user's own records
	// (a mismatch is indistinguishable from an unknown id).
	handleCP("POST /v1/consents/{id}/revoke", func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.RevokeConsent(r.Context(), r.PathValue("id"), r.URL.Query().Get("userId"))
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// Consent decision for a spawn edge: does a stored consent cover the
	// requested scopes right now? Always HTTP 200; IdentityServer's CIBA
	// notification hook calls this to decide auto-approve vs. prompt-the-user.
	// Scopes repeat: ?scope=a&scope=b.
	handleCP("GET /v1/consents/check", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		decision := registry.ConsentDecision{Granted: false, Reason: "no consent on record"}
		rec, ok, err := s.FindConsent(r.Context(), q.Get("userId"), q.Get("parentType"), q.Get("childType"))
		if storeErr(w, err) {
			return
		}
		if ok {
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
	handleCP("POST /v1/consent-requests", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UserID         string   `json:"userId"`
			ParentType     string   `json:"parentType"`
			ChildType      string   `json:"childType"`
			AgentID        string   `json:"agentId"`
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
			AgentID: req.AgentID, Scopes: req.Scopes, BindingMessage: req.BindingMessage, ExternalRef: req.ExternalRef,
		}

		ctx := r.Context()
		// Short-circuit: an existing covering consent means no human ask needed.
		rec, ok, err := s.FindConsent(ctx, req.UserID, req.ParentType, req.ChildType)
		if storeErr(w, err) {
			return
		}
		if ok && registry.EvaluateConsent(rec, req.Scopes, time.Now()).Granted {
			out, err := s.CreateApprovedConsentRequest(ctx, cr)
			if storeErr(w, err) {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(out)
			return
		}

		out, created, err := s.CreateConsentRequest(ctx, cr)
		if storeErr(w, err) {
			return
		}
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

	handleCP("GET /v1/consent-requests", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		reqs, err := s.ListConsentRequests(r.Context(), q.Get("userId"), q.Get("status"))
		if storeErr(w, err) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reqs)
	})

	handleCP("GET /v1/consent-requests/{id}", func(w http.ResponseWriter, r *http.Request) {
		cr, ok, err := s.GetConsentRequest(r.Context(), r.PathValue("id"))
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cr)
	})

	handleCP("POST /v1/consent-requests/{id}/approve", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Scopes []string `json:"scopes"`
		}
		// Body is optional — an empty/absent body approves the originally
		// requested scopes.
		_ = json.NewDecoder(r.Body).Decode(&body)
		cr, ok, err := s.ResolveConsentRequest(r.Context(), r.PathValue("id"), r.URL.Query().Get("userId"), true, body.Scopes)
		if storeErr(w, err) {
			return
		}
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cr)
	})

	handleCP("POST /v1/consent-requests/{id}/deny", func(w http.ResponseWriter, r *http.Request) {
		cr, ok, err := s.ResolveConsentRequest(r.Context(), r.PathValue("id"), r.URL.Query().Get("userId"), false, nil)
		if storeErr(w, err) {
			return
		}
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
		ctx := r.Context()
		rec, err := s.GetAgent(ctx, agentID)
		if storeErr(w, err) {
			return
		}
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
			parent, _ := s.GetAgent(ctx, cur.ParentID)
			if parent.AgentID == "" {
				break // missing parent — include what's resolvable
			}
			cur = parent
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"chain": chain})
	})

	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, r *http.Request) {
		types, err := s.ListTemplateTypes(r.Context())
		if storeErr(w, err) {
			return
		}
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
		// Version is a content hash so distinct override files report distinct
		// versions via GET /v1/schema (a fixed "custom" string would collide).
		sum := sha256.Sum256(b)
		return string(b), "sha256:" + hex.EncodeToString(sum[:])[:12], "override"
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

	// Registration auth is pluggable: the verifier identifies the caller
	// registering an agent. It follows the platform-wide ATTESTOR selector by
	// default (spiffe -> spiffe-svid, aws-sts -> oidc), but REGISTRANT_VERIFIER
	// can override it explicitly for mixed-attestor deployments. Whatever is
	// chosen, its AgentID derivation MUST match the IdentityServer verifier's,
	// or minted tokens won't line up with registry records.
	var attestorDefault string
	switch a := getEnv("ATTESTOR", "spiffe"); a {
	case "spiffe":
		attestorDefault = "spiffe-svid"
	case "aws-sts":
		attestorDefault = "aws-sts"
	default:
		log.Fatalf("unknown ATTESTOR %q", a)
	}
	var verifier registrant.Verifier
	switch v := getEnv("REGISTRANT_VERIFIER", attestorDefault); v {
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
	case "aws-sts":
		// Registration credential is a presigned STS GetCallerIdentity request;
		// the verifier replays it and derives AgentID from the session name.
		verifier = registrant.NewAwsStsVerifier(nil)
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

	// Control-plane auth is pluggable, mirroring registration auth: it gates the
	// consent-lifecycle endpoints, which only platform services call. Default
	// "none" runs them open (local demo); "shared-secret" requires a static
	// bearer; "oidc" validates a client-credentials access token against a
	// configured JWKS (any IdP), enforcing audience + scope.
	cpAuth := buildControlPlaneAuth(ctx)

	s := newStore() // already seeded with the embedded default schema
	if schemaSource != "default" {
		// Only an override needs a re-parse; the default was parsed in newStore.
		s.setSchema(activeSchema, schemaVersion, schemaSource)
	}
	log.Printf("registry listening on :8080 (schema source=%s version=%s)", schemaSource, schemaVersion)
	log.Fatal(http.ListenAndServe(":8080", buildMux(s, sdb, verifier, cpAuth)))
}

// buildControlPlaneAuth selects the consent-endpoint authenticator from
// CONTROL_PLANE_AUTH (none|shared-secret|oidc). It log.Fatals on a
// misconfiguration so a deployment that intends to gate consent fails loudly
// rather than silently running open.
func buildControlPlaneAuth(ctx context.Context) controlplane.Authenticator {
	switch v := getEnv("CONTROL_PLANE_AUTH", "none"); v {
	case "none":
		return controlplane.AllowAll()
	case "shared-secret":
		token := getEnv("CONTROL_PLANE_TOKEN", "")
		if token == "" {
			log.Fatalf("CONTROL_PLANE_TOKEN required when CONTROL_PLANE_AUTH=shared-secret")
		}
		return controlplane.NewSharedSecret(token)
	case "oidc":
		cfg := controlplane.OIDCConfig{
			JWKSURL:               getEnv("CONTROL_PLANE_OIDC_JWKS_URL", ""),
			Audience:              getEnv("CONTROL_PLANE_OIDC_AUDIENCE", "registry"),
			RequiredScope:         getEnv("CONTROL_PLANE_OIDC_SCOPE", "registry.consent"),
			InsecureSkipTLSVerify: getEnv("CONTROL_PLANE_OIDC_INSECURE_SKIP_TLS_VERIFY", "false") == "true",
		}
		if cfg.JWKSURL == "" {
			log.Fatalf("CONTROL_PLANE_OIDC_JWKS_URL required when CONTROL_PLANE_AUTH=oidc")
		}
		auth, err := controlplane.NewOIDC(ctx, cfg)
		if err != nil {
			log.Fatalf("control-plane OIDC init: %v", err)
		}
		return auth
	default:
		log.Fatalf("unknown CONTROL_PLANE_AUTH %q", v)
		return nil // unreachable
	}
}
