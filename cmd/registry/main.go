// cmd/registry/main.go
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/orgdirectory"
	"github.com/spawnly/platform/internal/orgdirectory/keycloak"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/registry/dynamostore"
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

// Bumped to v2 when the embedded schema gained the org multi-tenancy
// definitions (user/organization) and the agent owner/org relations +
// view/manage permissions. The version is informational (GET /v1/schema + boot
// log); WriteSchema is applied unconditionally on boot regardless of it.
const defaultSchemaVersion = "v2"

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

	// orgs holds organization records keyed by OrgID. Membership is NOT here —
	// Keycloak owns who's in an org, SpiceDB owns authorization; this stores only
	// the org's own record (name, settings, optional attestation anchor).
	orgs map[string]registry.Organization

	// consentRequests holds brokered consent asks by ID (Phase 5b). openRequests
	// indexes the single open (pending) request per edge by consentKey, so
	// creating a request while one is pending returns the existing one rather
	// than firing a duplicate prompt.
	consentRequests map[string]registry.ConsentRequest
	openRequests    map[string]string // consentKey -> pending request ID

	// Active SpiceDB schema (text/version/source + parsed validation model).
	// Embedded so SetSchema/Schema/ValidateTemplate are shared verbatim with the
	// DynamoDB store — the schema is boot-time config both stores keep in memory,
	// NOT persisted data (see registry.SchemaState). Seeded with the embedded
	// default in newStore; main() re-applies the loaded active schema (default or
	// SPICEDB_SCHEMA_PATH override).
	registry.SchemaState
}

func newStore() *store {
	s := &store{
		templates:       map[string]registry.AgentTemplate{},
		agents:          map[string]registry.AgentRecord{},
		events:          map[string][]events.Event{},
		consents:        map[string]registry.ConsentRecord{},
		consentRequests: map[string]registry.ConsentRequest{},
		openRequests:    map[string]string{},
		orgs:            map[string]registry.Organization{},
	}
	s.SetSchema(defaultSchema, defaultSchemaVersion, "default")
	return s
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

// putOrg upserts an organization record (replaces the whole record, matching
// the agent/template write semantics).
func (s *store) putOrg(org registry.Organization) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.orgs[org.OrgID] = org
}

// getOrg returns the org and whether it was found.
func (s *store) getOrg(id string) (registry.Organization, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	org, ok := s.orgs[id]
	return org, ok
}

// listOrgs returns all organization records.
func (s *store) listOrgs() []registry.Organization {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]registry.Organization, 0, len(s.orgs))
	for _, org := range s.orgs {
		out = append(out, org)
	}
	return out
}

// deleteOrg removes an org, returning whether one was found.
func (s *store) deleteOrg(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.orgs[id]; !ok {
		return false
	}
	delete(s.orgs, id)
	return true
}

// appendEvent stamps and stores an event, returning the stored copy. It returns
// the value it wrote rather than having callers re-read the tail of the list,
// so the AppendEvent contract ("returns the stored event") holds without a
// second locked read — and so a durable backend can return what it persisted
// instead of relying on an in-process append order.
func (s *store) appendEvent(agentID string, e events.Event) events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	e.ID = eventID(now.UnixNano())
	e.Timestamp = now
	s.events[agentID] = append(s.events[agentID], e)
	return e
}

// eventID mints a genuinely-unique, ordering-stable event id shared by both Store
// backends: a %020d zero-padded nanosecond so lexical id order == chronological
// order, plus a random suffix so two appends in the SAME nanosecond still get
// distinct ids. The DynamoDB store builds the SAME %020d-nano#suffix into its sort
// key, so a consumer that keys off (or sorts by) the event id sees identical
// ordering whichever backend served it. The dashboard's incremental renderer keys
// on this id, so uniqueness is load-bearing — a bare UnixNano collided within a
// nanosecond and could hide/duplicate a row.
func eventID(nano int64) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%020d-%s", nano, hex.EncodeToString(b))
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

// consentKey / openRequestKey delegate to the shared registry key builders so the
// in-memory store and the DynamoDB store serialise edges identically (single
// source of truth in the registry contract).
func consentKey(userID, parentType, childType string) string {
	return registry.ConsentEdgeKey(userID, parentType, childType)
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

func openRequestKey(userID, parentType, childType, agentID string) string {
	return registry.OpenRequestKey(userID, parentType, childType, agentID)
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

// approvePendingConsentRequest flips a still-pending request to approved with NO
// grant write — the status-only pivot the read-time backstop uses when a covering
// grant already exists. Idempotent: an already-resolved request is returned
// unchanged. It deliberately does NOT touch s.consents, so it can never un-revoke
// or reshape the covering grant (see the Store interface doc).
func (s *store) approvePendingConsentRequest(id string) (registry.ConsentRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cr, ok := s.consentRequests[id]
	if !ok {
		return registry.ConsentRequest{}, false
	}
	if cr.Status != registry.ConsentPending {
		return cr, true // already resolved — idempotent
	}
	now := time.Now()
	cr.Status = registry.ConsentApproved
	cr.ResolvedAt = &now
	s.consentRequests[id] = cr
	delete(s.openRequests, openRequestKey(cr.UserID, cr.ParentType, cr.ChildType, cr.AgentID))
	return cr, true
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
// Returns (nil, nil) when the id is unknown; a non-nil error means a backend read
// failed and the caller MUST fail the request rather than act on a partial walk —
// a truncated subtree would silently leave descendants un-revoked (still holding
// live authority), so the durable-backend error path fails loud (5xx).
func subtree(ctx context.Context, s registry.Store, id string) ([]string, error) {
	root, err := s.GetAgent(ctx, id)
	if err != nil {
		return nil, err
	}
	if root.AgentID == "" {
		return nil, nil
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
		children, err := s.ListChildren(ctx, cur)
		if err != nil {
			return nil, err
		}
		for _, c := range children {
			queue = append(queue, c.AgentID)
		}
	}
	return out, nil
}

// SpiceDB agent permissions checked for human lifecycle authority (schema.zed):
// view = owner + org->admin_agents (read: subtree/events/logs/chain);
// manage = owner + org->admin_agents (mutate: revoke/resume/dismiss/kill/...).
// Since tenant-visibility Phase 2 view == manage (both owner + org admin): a
// plain org member sees ONLY the agents they own.
const (
	actionView   = "view"
	actionManage = "manage"
)

// agentAllowed reports whether the agent exists and whether userId is allowed the
// given action (actionView / actionManage) on it — the Phase-4 authorization flip
// from pure userId-equality to SpiceDB org checks. Two paths:
//
//   - ORG-LESS agent (record OrgID == ""): today's equality, decided from the
//     record alone with NO SpiceDB round-trip. Global agents (incl. any whose
//     owner/org tuples predate this phase) keep working exactly as before, and
//     the existing e2e — all org-less — stays on this path unchanged.
//   - ORG-SCOPED agent (record OrgID != ""): SpiceDB is authoritative via
//     CheckPermission on "agent:<id>" for the action. The subject userId is
//     normalized with spicedb.EncodeObjectID so it matches the object id written
//     on the owner/membership tuples (registry.ValidUserID admits '. : @', which
//     SpiceDB's object-id charset rejects raw).
//
// A STORE read error is PROPAGATED (exists=false, err set) so the handler surfaces
// a retryable 5xx — mapping it to 404 would falsely tell a caller the agent is
// gone. A SpiceDB CheckPermission error, by contrast, FAILS CLOSED: it is logged
// and returns allowed=false (a 404 for the caller), never a 5xx oracle and never
// an open grant — matching the recorded agentOwnedBy follow-up.
func agentAllowed(ctx context.Context, s registry.Store, sdb spicedb.Client, id, userId, action string) (allowed, exists bool, err error) {
	rec, err := s.GetAgent(ctx, id)
	if err != nil {
		return false, false, err
	}
	if rec.AgentID == "" {
		return false, false, nil
	}
	if userId == "" {
		return false, true, nil // no asserted identity never owns / is never a member
	}
	if rec.OrgID == "" {
		return rec.UserID == userId, true, nil
	}
	ok, cerr := sdb.CheckPermission(ctx, "agent:"+id, action, "user:"+spicedb.EncodeObjectID(userId))
	if cerr != nil {
		// Fail closed: deny (404) and log, rather than 5xx (which would leak that
		// the id exists) or fail open. The org tuples are the source of truth; a
		// SpiceDB hiccup must not grant access.
		log.Printf("agentAllowed: spicedb %s check for agent %s user %s failed (fail-closed deny): %v", action, id, userId, cerr)
		return false, true, nil
	}
	return ok, true, nil
}

// depth returns how many agents are in the lineage from id up to the root,
// counting id itself: a top-level agent is depth 1, its child depth 2, and so
// on. Used to enforce a template's maxDepth at spawn time. Unknown ids are 0.
//
// A backend read error is PROPAGATED rather than swallowed: this is FAIL-OPEN if
// ignored — a read failure part-way up the lineage would UNDERCOUNT the depth,
// and the caller's maxDepth gate would then admit a spawn it should have rejected.
// So a per-hop read error aborts the walk and the spawn-policy handler fails the
// request (5xx) instead of ruling on a truncated lineage.
func depth(ctx context.Context, s registry.Store, id string) (int, error) {
	d := 0
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		rec, err := s.GetAgent(ctx, cur)
		if err != nil {
			return 0, err
		}
		if rec.AgentID == "" {
			break
		}
		seen[cur] = true // cycle guard
		d++
		cur = rec.ParentID
	}
	return d, nil
}

// backstopPendingConsentRequest is the read-time correctness net for the
// approve-sweep's residual GSI-lag miss. On a durable (DynamoDB) backend the
// approve-sweep reads an EVENTUALLY-consistent index, so a sibling pending
// request committed microseconds before an approve can be skipped by the sweep
// and stay pending until CIBA expiry/renewal. Whenever a pending request is READ
// back (the poller's GET /v1/consent-requests/{id}), re-check it against the
// STRONGLY-consistent FindConsent + EvaluateConsent — the SAME covered-grant
// logic the create-path short-circuit uses — and, if a covering unexpired grant
// already exists, resolve it approved right here.
//
// It lives at the handler (neutral-contract) level so BOTH stores get it: on the
// in-memory store the sweep is deterministic so this is a cheap no-op-ish check;
// on DynamoDB it closes the lag window without the handler knowing the backend.
//
// It resolves via the STATUS-ONLY ApprovePendingConsentRequest — it flips only the
// request, never the grant. The grant is the precondition (we just confirmed it
// covers the request), so touching it would be wrong and would open real races: a
// grant-rewriting resolve could un-revoke a grant killed between our FindConsent
// and the flip, clobber a concurrent broader re-grant, or (if the template TTL was
// since removed) re-stamp an expiring grant as permanent. The status-only flip
// eliminates all three: it is conditional (approve iff still pending), idempotent,
// and grant-blind.
func backstopPendingConsentRequest(ctx context.Context, s registry.Store, cr registry.ConsentRequest) (registry.ConsentRequest, error) {
	if cr.Status != registry.ConsentPending {
		return cr, nil
	}
	rec, ok, err := s.FindConsent(ctx, cr.UserID, cr.ParentType, cr.ChildType)
	if err != nil {
		return cr, err
	}
	if !ok || !registry.EvaluateConsent(rec, cr.Scopes, time.Now()).Granted {
		return cr, nil // no covering grant — legitimately still pending
	}
	resolved, resolvedOK, err := s.ApprovePendingConsentRequest(ctx, cr.ID)
	if err != nil {
		return cr, err
	}
	if !resolvedOK {
		return cr, nil // vanished between read and flip (not possible today) — return as-is
	}
	return resolved, nil
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

// templateInScope reports whether agentType exists AND belongs to scopeOrg
// (Phase 6 org-scoped template management). It is the shared guard for the
// org-admin-scoped PATCH path (DELETE checks the equivalent inline after its own
// GetTemplate). On any failure it writes the response (500 on a store error, 404
// when the template is unknown OR belongs to a different org — a single 404 so an
// org admin can't distinguish "no such template" from "template exists in another
// org", no cross-org existence oracle) and returns false; the caller returns.
func templateInScope(w http.ResponseWriter, ctx context.Context, s registry.Store, agentType, scopeOrg string) bool {
	t, ok, err := s.GetTemplate(ctx, agentType)
	if storeErr(w, err) {
		return false
	}
	if !ok || t.OrgID != scopeOrg {
		http.Error(w, "not found", http.StatusNotFound)
		return false
	}
	return true
}

// filterTemplatesVisible returns the templates SPAWNABLE by a caller whose active
// org is org (Phase 6): every global template (OrgID == "", the seeded catalog)
// plus templates scoped to org. An org-less caller (org == "") sees global only.
// This is the "global + own org" visibility used by the spawn dropdown / bare list.
func filterTemplatesVisible(tpls []registry.AgentTemplate, org string) []registry.AgentTemplate {
	out := make([]registry.AgentTemplate, 0, len(tpls))
	for _, t := range tpls {
		if t.OrgID == "" || t.OrgID == org {
			out = append(out, t)
		}
	}
	return out
}

// filterTemplatesOwnOrg returns ONLY the templates scoped to org (excludes global)
// — the org-admin Agent Types management view, which manages the org's own
// catalog, never the global one.
func filterTemplatesOwnOrg(tpls []registry.AgentTemplate, org string) []registry.AgentTemplate {
	out := make([]registry.AgentTemplate, 0, len(tpls))
	for _, t := range tpls {
		if t.OrgID == org {
			out = append(out, t)
		}
	}
	return out
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

// orgIsDisabled reports whether orgID names an org whose record is SUSPENDED
// (Disabled == true) — the org-lifecycle spawn guard (signup-lifecycle Phase 4).
// An empty orgID (an org-less / global agent) is never disabled. An unknown org is
// treated as not-disabled (the create path validates org existence elsewhere; a
// missing record must not spuriously block a global spawn). A store error is
// surfaced so the caller fails CLOSED rather than silently spawning into a disabled
// org on a transient read failure.
func orgIsDisabled(ctx context.Context, s registry.Store, orgID string) (bool, error) {
	if orgID == "" {
		return false, nil
	}
	org, ok, err := s.GetOrg(ctx, orgID)
	if err != nil {
		return false, err
	}
	return ok && org.Disabled, nil
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
// Returns (revoked?, error). A non-nil error (agent read OR the status write
// failed) must abort the cascade with a 5xx rather than silently skip the node:
// a swallowed read/write error here would drop a descendant's revocation while
// still reporting success, leaving it with live authority. The SpiceDB tuple drop
// and the audit event stay best-effort (logged) — the status flip is the durable
// pivot permission checks key off, and an external-system or audit-log hiccup must
// not un-revoke an agent whose record already flipped.
func revokeNode(ctx context.Context, s registry.Store, sdb spicedb.Client, id string) (bool, error) {
	rec, err := s.GetAgent(ctx, id)
	if err != nil {
		return false, err
	}
	if rec.Status != "active" {
		return false, nil // idempotent: never clobber a node that already exited
	}
	if _, err := s.UpdateAgentStatus(ctx, id, "revoked"); err != nil {
		return false, err
	}
	// Phase 5a: revoke is reversible, so drop only the agent's `enabled` status
	// tuple — a single write regardless of template size. The template relations
	// are deliberately left in place so resumeNode can re-enable in O(1) without
	// re-deriving them from a template that may have since changed or been
	// deleted. Permission denial is immediate: `work_on = agent & agent->enabled`
	// fails the intersection the moment this tuple is gone.
	if err := sdb.DeleteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id); err != nil {
		log.Printf("spicedb revoke (enabled-tuple delete) error for %s: %v", id, err)
	}
	if _, err := s.AppendEvent(ctx, id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_revoked",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	}); err != nil {
		log.Printf("append agent_revoked event error for %s: %v", id, err)
	}
	return true, nil
}

// revokeCascadeRoots runs the shared revoke cascade (subtree + revokeNode) over
// every ROOT agent selected by root — a root being an agent for which root(a) is
// true whose parent is not ALSO in scope (a child of an in-scope parent is already
// covered by that parent's cascade, and revokeNode is idempotent regardless). It
// is the single implementation behind both org-suspension (suspendOrgAgents) and
// owner-suspension (suspendUserAgents / the user-delete org-less revoke).
//
// Two predicates, deliberately: scope defines the membership set used for the
// parent-owned skip and is computed over ALL agents up front, while root decides
// which of those to START a cascade at. They coincide for the org case (scope ==
// root == "in this org"), but the owner case narrows root MORE tightly than scope
// (e.g. delete revokes only the user's ORG-LESS roots, root = owned && orgId==""),
// yet the skip must still see the user's FULL owned set so a narrowed root whose
// parent is an out-of-root owned agent is correctly treated as covered — folding
// the two into one predicate would change that edge. root callers pass a predicate
// that already implies scope. A backend read/write error is returned so the caller
// fails loud rather than leaving some agents un-revoked.
func revokeCascadeRoots(ctx context.Context, s registry.Store, sdb spicedb.Client, scope, root func(registry.AgentRecord) bool) error {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return err
	}
	scopeSet := map[string]bool{}
	var roots []registry.AgentRecord
	for _, a := range agents {
		if scope(a) {
			scopeSet[a.AgentID] = true
		}
	}
	for _, a := range agents {
		if root(a) {
			roots = append(roots, a)
		}
	}
	return cascadeOverRoots(ctx, s, sdb, roots, scopeSet)
}

// cascadeOverRoots revokes the full subtree of each root agent whose parent is not
// ALSO in scopeSet (a child of an in-scope parent is already covered by that
// parent's cascade, and revokeNode is idempotent regardless). It is the single
// cascade implementation shared by revokeCascadeRoots (org/user suspension) and
// the removal-disposition auto-revoke path, so a future change to the cascade
// lands in exactly one place. A backend read/write error is returned so the caller
// fails loud rather than leaving some agents un-revoked.
func cascadeOverRoots(ctx context.Context, s registry.Store, sdb spicedb.Client, roots []registry.AgentRecord, scopeSet map[string]bool) error {
	for _, a := range roots {
		if a.ParentID != "" && scopeSet[a.ParentID] {
			continue // covered by the in-scope parent's cascade
		}
		nodes, err := subtree(ctx, s, a.AgentID)
		if err != nil {
			return err
		}
		for _, nid := range nodes {
			if _, err := revokeNode(ctx, s, sdb, nid); err != nil {
				return err
			}
		}
	}
	return nil
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
// Returns (resumed?, error). Like revokeNode, a backend read/write error aborts
// the cascade with a 5xx rather than silently skipping the node.
func resumeNode(ctx context.Context, s registry.Store, sdb spicedb.Client, id string) (bool, error) {
	rec, err := s.GetAgent(ctx, id)
	if err != nil {
		return false, err
	}
	if rec.Status != "revoked" {
		return false, nil
	}
	if _, err := s.UpdateAgentStatus(ctx, id, "active"); err != nil {
		return false, err
	}
	if err := sdb.WriteRelationship(ctx, "agent:"+id, "enabled", "agent:"+id); err != nil {
		log.Printf("spicedb resume (enabled-tuple write) error for %s: %v", id, err)
	}
	if _, err := s.AppendEvent(ctx, id, events.Event{
		Source:  events.SourceRegistry,
		Type:    "agent_resumed",
		Payload: mustMarshal(map[string]string{"agentId": id}),
	}); err != nil {
		log.Printf("append agent_resumed event error for %s: %v", id, err)
	}
	return true, nil
}

// applyRemovalDisposition settles a removed member's running agents per the org's
// removal-disposition setting (Phase 6). It runs AFTER the member's Keycloak
// membership and SpiceDB membership tuples are gone (the DELETE members handler),
// so the ex-member has ALREADY lost all access and can no longer mint into the org
// — this only decides the fate of the agents they OWNED in the org:
//
// In BOTH dispositions the ex-member's OWNER tuple on each of their org agents is
// dropped, so their lifecycle authority (view/manage via the owner term) dies with
// their membership — the spec's "the ex-member loses ALL access" (a removed member
// must not be able to keep reading/resuming an org agent just because they spawned
// it). What differs is the fate of the agent itself:
//
//   - auto-revoke: additionally run the existing revoke cascade (subtree +
//     revokeNode) over each owned ROOT (an owned agent whose parent is not itself
//     owned), so the ex-member's agents and everything they spawned stop acting.
//   - orphan-to-admin (default, non-destructive): the agent keeps running; the
//     record keeps UserID for history; org admins retain manage via org->admin_agents.
//
// Owner-tuple drops + cascade roots are strictly the ex-member's OWN agents in
// THIS org (OrgID == org && UserID == user). The auto-revoke cascade only ever
// STARTS from an owned root, so it never revokes an unrelated agent — but, exactly
// like a normal owner-initiated revoke, it does follow ParentID into descendants
// the ex-member spawned even if a descendant is owned by another member (that is
// the ex-member's delegation subtree, within their revoke authority). A backend
// error is returned so the handler fails loud (the whole remove is retryable +
// idempotent).
func applyRemovalDisposition(ctx context.Context, s registry.Store, sdb spicedb.Client, org, user, disposition string) error {
	agents, err := s.ListAgents(ctx)
	if err != nil {
		return err
	}
	var owned []registry.AgentRecord
	ownedSet := map[string]bool{}
	for _, a := range agents {
		if a.OrgID == org && a.UserID == user {
			owned = append(owned, a)
			ownedSet[a.AgentID] = true
		}
	}
	if len(owned) == 0 {
		return nil
	}
	// Always sever the ex-member's ownership of their org agents (best-effort: the
	// membership tuple is already gone, so a stray owner tuple only over-grants to a
	// subject that can no longer authenticate; a retry re-runs this idempotently).
	sub := "user:" + spicedb.EncodeObjectID(user)
	for _, a := range owned {
		if derr := sdb.DeleteRelationship(ctx, "agent:"+a.AgentID, "owner", sub); derr != nil {
			log.Printf("removal disposition: drop owner tuple for %s: %v", a.AgentID, derr)
		}
	}
	if disposition == registry.RemovalAutoRevoke {
		// The same root-cascade as org/user suspension, over this ex-member's owned
		// roots (owned already lists them; ownedSet is the parent-skip scope).
		return cascadeOverRoots(ctx, s, sdb, owned, ownedSet)
	}
	// orphan-to-admin: audit each orphaned agent (revokeNode emits its own event on
	// the auto-revoke path).
	for _, a := range owned {
		if _, aerr := s.AppendEvent(ctx, a.AgentID, events.Event{
			Source:  events.SourceRegistry,
			Type:    "agent_orphaned",
			Payload: mustMarshal(map[string]string{"agentId": a.AgentID, "orgId": org, "formerOwner": user}),
		}); aerr != nil {
			log.Printf("removal disposition orphan: append event for %s: %v", a.AgentID, aerr)
		}
	}
	return nil
}

// rejectDuplicateUserID is a defense-in-depth guard for every userId-scoped
// ownership/consent check in the registry. Those handlers read userId with
// url.Values.Get, which returns only the FIRST value, so a request carrying
// userId more than once — e.g. a smuggled `?userId=victim` sitting ahead of a
// caller-appended real one — would silently scope to the attacker-chosen value
// (a confused deputy). A well-formed caller never sends userId twice, so a
// duplicate is a bug or an attack: reject it (400) rather than guess which is
// authoritative. This makes the smuggle impossible at the source, independent of
// how any upstream caller builds the query string.
func rejectDuplicateUserID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vals := r.URL.Query()["userId"]
		if len(vals) > 1 {
			http.Error(w, "duplicate userId query parameter", http.StatusBadRequest)
			return
		}
		// Structural charset guard at the boundary: the single userId query value
		// flows into store keys and begins_with prefix scoping, so a '|' or '#'
		// would corrupt an edge key or widen a per-user query. Reject a malformed
		// value here — for EVERY query-param userId route at once — rather than
		// trusting subjects to be separator-free. Empty is allowed (unscoped).
		if len(vals) == 1 && !rejectInvalidUserID(w, vals[0]) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rejectInvalidUserID writes a 400 and returns false when userId is non-empty and
// structurally invalid (see registry.ValidUserID); an empty userId is allowed
// (the "unscoped"/"no identity" case every userId-scoped handler already treats
// as denying ownership). Callers use `if !rejectInvalidUserID(w, id) { return }`.
func rejectInvalidUserID(w http.ResponseWriter, userID string) bool {
	if userID != "" && !registry.ValidUserID(userID) {
		http.Error(w, "invalid userId", http.StatusBadRequest)
		return false
	}
	return true
}

func buildMux(s registry.Store, sdb spicedb.Client, verifier registrant.Verifier, cpAuth controlplane.Authenticator, dir orgdirectory.Directory) http.Handler {
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

	// Org onboarding + membership control-plane API (multi-tenancy Phase 3). All
	// org/membership mutation flows through here (registry = the single dual-write
	// choke point over Keycloak + SpiceDB); every route is cp-gated.
	// ORG_APEX_DOMAIN (org-subdomains Phase 2): the apex under which each org's
	// satellite lives (e.g. spawnly.run / localtest.me). Empty = feature off →
	// org create/delete skip registering satellite redirect URIs on the dashboard
	// client.
	// MAX_ORGS (signup-lifecycle Phase 2): a global ceiling on self-serve org
	// creation. Unset/empty/0/non-positive/unparseable ⇒ unlimited (0 sentinel);
	// only a well-formed positive integer arms the cap, enforced on the
	// ?selfServe=1 path in registerOrgRoutes. The platform-admin onboarding path
	// is deliberately NOT gated (admin bypass).
	maxOrgs := 0
	if v := strings.TrimSpace(os.Getenv("MAX_ORGS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxOrgs = n
		} else {
			log.Printf("ignoring invalid MAX_ORGS=%q (treating as unlimited)", v)
		}
	}
	registerOrgRoutes(mux, s, sdb, dir, getEnv("ORG_APEX_DOMAIN", ""), maxOrgs, getEnv("MASTER_ADMIN_USERNAME", "root"), cp)

	// Master-admin user lifecycle (signup-lifecycle Phase 3): list/disable/enable/
	// delete users through the same cp-gated single choke point. MASTER_ADMIN_USERNAME
	// (default "root") names the platform master-admin account the root lockout guard
	// refuses to disable or delete.
	registerUserRoutes(mux, s, sdb, dir, getEnv("MASTER_ADMIN_USERNAME", "root"), cp)

	// Template management (create/disable/delete) is a control-plane operation,
	// so it sits behind the same control-plane authenticator as the consent
	// lifecycle. Under the default AllowAll authenticator it runs open.
	mux.HandleFunc("POST /v1/templates", cp(func(w http.ResponseWriter, r *http.Request) {
		var t registry.AgentTemplate
		if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Org scope (Phase 6) is stamped from the TRUSTED ?orgId= the orchestrator
		// derives from the caller's token claim (the withTemplateOrg pattern),
		// NEVER from the request body — a dashboard-created template is bound to the
		// org it was created in, and a body OrgID must not be able to forge that.
		// Empty (realm-admin create, or terraform) = a global template. Validate the
		// charset so it stays a safe SpiceDB/DNS-label object id like every other org id.
		t.OrgID = r.URL.Query().Get("orgId")
		if t.OrgID != "" && !validOrgID.MatchString(t.OrgID) {
			http.Error(w, "invalid orgId", http.StatusBadRequest)
			return
		}
		// Scope-integrity guard (Phase 6 security): templates share ONE global name
		// namespace (keyed by AgentType) and PutTemplate is an unconditional upsert,
		// so without this an org admin could re-POST an existing name and silently
		// STEAL it into their org — hijacking the global catalog (OrgID "") or another
		// org's template. On a name collision:
		//   • org-scoped create (?orgId set): the existing template MUST already be in
		//     the SAME org, else 409 — an org admin may only edit their own org's
		//     template of that name, never a global or cross-org one.
		//   • realm-admin create (no ?orgId, global authority): PRESERVE the existing
		//     scope rather than de-scoping an org template into the global catalog (a
		//     realm admin's edit of an org template keeps it org-scoped). A brand-new
		//     name stays global as intended.
		if existing, ok, err := s.GetTemplate(r.Context(), t.AgentType); storeErr(w, err) {
			return
		} else if ok {
			if t.OrgID == "" {
				t.OrgID = existing.OrgID
			} else if existing.OrgID != t.OrgID {
				http.Error(w, "agent type already exists", http.StatusConflict)
				return
			}
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
		// Org-scoped management (Phase 6): when the caller is org-scoped (the
		// orchestrator forwarded ?orgId= for an org admin), the target template MUST
		// belong to that org — an org admin can neither manage the global catalog
		// (OrgID "") nor another org's templates. A realm admin forwards no orgId and
		// bypasses this (global authority). A mismatch (or unknown) reads as the same
		// 404 the update path returns, so there is no cross-org existence oracle.
		if scopeOrg := r.URL.Query().Get("orgId"); scopeOrg != "" {
			if !templateInScope(w, r.Context(), s, agentType, scopeOrg) {
				return
			}
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
		// Org-scoped management (Phase 6): an org admin (?orgId= set) may delete only
		// their own org's templates; realm admin (no orgId) may delete any. Mismatch
		// reads as 404 (no cross-org oracle) — checked before the disabled-state 409
		// so a wrong-org caller can't even learn the template's status.
		if scopeOrg := r.URL.Query().Get("orgId"); scopeOrg != "" && t.OrgID != scopeOrg {
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

	// Single-template fetch returns the FULL template record (runtimeSpec,
	// authzTemplate, delegation policy, oauthScopes), so it is control-plane
	// gated like its PATCH/DELETE siblings — a public caller must not read a
	// template's authz relations, grantable scopes, or a disabled template's
	// body. The legitimate callers all carry the control-plane bearer: the
	// orchestrator (regClient built with the control-plane token source), the
	// operator, and the terraform provider. Non-sensitive discovery stays open
	// via the bare `GET /v1/templates` (active names) and `?detail=spawn`
	// ({agentType, requiresTenant}) list routes.
	mux.HandleFunc("GET /v1/templates/", cp(func(w http.ResponseWriter, r *http.Request) {
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
	}))

	// Internal pre-registration endpoint. Called by the orchestrator at spawn time
	// so the agent appears in the UI immediately with "pending" status rather than
	// waiting for the sidecar to start. Its ONLY caller is the orchestrator, which
	// holds the control-plane bearer, so it sits behind cpAuth (was unauthenticated
	// — a hole: it set the record's ParentID, which the events gate trusts to
	// authorize child->parent audit posts, so any caller could plant a forged
	// lineage before an agent self-registered). Under CONTROL_PLANE_AUTH=none this
	// runs open, so kind is unaffected.
	handleCP("POST /v1/agents/preregister", func(w http.ResponseWriter, r *http.Request) {
		var rec registry.AgentRecord
		if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if rec.AgentID == "" {
			http.Error(w, "agentId required", http.StatusBadRequest)
			return
		}
		// The preregistered userId (set by the orchestrator from the spawner's
		// token sub) becomes the agent record's owner and feeds ownership scoping —
		// validate its charset at ingest.
		if !rejectInvalidUserID(w, rec.UserID) {
			return
		}
		// The preregistered orgId becomes a SpiceDB object id ("organization:"+id)
		// on the agent's org tuple — constrain its charset at ingest exactly like
		// agentId (this endpoint is the ONLY ingress that accepts orgId; the
		// self-register path derives it from this record). Phase 3's org
		// onboarding tightens org ids further to DNS labels.
		if rec.OrgID != "" && !validAgentID.MatchString(rec.OrgID) {
			http.Error(w, "invalid orgId", http.StatusBadRequest)
			return
		}
		// Spawn guard (signup-lifecycle Phase 4): refuse to preregister an agent into
		// a SUSPENDED org — the orchestrator's spawn ingress, so a spawn into a
		// disabled org fails fast here rather than materializing a pending record.
		if disabled, derr := orgIsDisabled(r.Context(), s, rec.OrgID); storeErr(w, derr) {
			return
		} else if disabled {
			http.Error(w, "organization is disabled", http.StatusForbidden)
			return
		}
		// Placement (Phase 10) is trusted from this cp-gated body (the orchestrator's
		// remote-registration path sets it), but constrain it to the known values so
		// a typo can never yield a record that neither the sweep nor the UI expects.
		if rec.Placement != "" && rec.Placement != registry.PlacementK8s && rec.Placement != registry.PlacementRemote {
			http.Error(w, "invalid placement", http.StatusBadRequest)
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
		// userId (token-derived by the orchestrator, or empty for a global agent)
		// is stored as the record owner — validate its charset at ingest.
		if !rejectInvalidUserID(w, req.UserID) {
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
		// ParentID is IMMUTABLE once the record exists. A self-registering sidecar
		// only proves its OWN agentID (SVID-pinned above), not its lineage, so it
		// must not be able to (re)point ParentID from the request body — the events
		// gate authorizes a child->parent audit post off the stored ParentID, so a
		// forgeable parent would let any agent plant parentId=victim and post events
		// onto the victim. The trusted set-once happens at preregister (cpAuth) or,
		// for an un-preregistered brand-new self-register, this first write.
		// ParentID, UserID (owner), and OrgID are all set-once — at preregister
		// (cpAuth, orchestrator-asserted) or, for an un-preregistered brand-new
		// self-register, at this first write — and IMMUTABLE thereafter. A
		// self-registering sidecar proves ONLY its own agentId (SVID-pinned),
		// never its lineage, owner, or org, so none of the three may be
		// (re)pointed from the request body once the record exists: a forgeable
		// owner would let a sidecar rewrite the record's owner (and, from Phase 4
		// on, the agent:<id>#owner authz tuple) to an arbitrary user; a forgeable
		// org would let it claim another org's agents.
		parentID := req.ParentID
		userID := req.UserID
		orgID := existing.OrgID
		// Placement is set-once at preregistration and immutable (like ParentID/
		// UserID/OrgID): a self-registering sidecar proves only its SVID-pinned
		// agentId, never where it runs, so it must NOT be able to declare itself
		// remote (dodging the operator's stale sweep) or flip a remote record to
		// k8s. It is never read from the request body — only carried from the
		// preregistered record.
		placement := existing.Placement
		if existing.AgentID != "" {
			switch existing.Status {
			case "completed", "failed", "revoked":
				http.Error(w, fmt.Sprintf("agent %s is %s; re-registration refused", agentID, existing.Status),
					http.StatusConflict)
				return
			}
			parentID = existing.ParentID
			userID = existing.UserID
		}
		// Spawn guard (signup-lifecycle Phase 4), defense in depth: refuse a
		// self-registration whose (server-derived, preregistered) OrgID belongs to a
		// SUSPENDED org. Covers a token minted before the org was disabled — the
		// sidecar's registry record was preregistered into the org while it was still
		// enabled, and this refuses the now-late self-register. (An un-preregistered
		// brand-new self-register carries no OrgID, so it is org-less and unaffected;
		// org-scoped agents — including remote STS-anchored ones — are always
		// preregistered by the orchestrator, where the preregister guard also bites.)
		if disabled, derr := orgIsDisabled(ctx, s, orgID); storeErr(w, derr) {
			return
		} else if disabled {
			http.Error(w, "organization is disabled", http.StatusForbidden)
			return
		}

		rec := registry.AgentRecord{
			AgentID:      agentID,
			AgentType:    req.AgentType,
			TenantID:     req.TenantID,
			UserID:       userID,
			Status:       "active",
			Lifecycle:    tpl.Runtime.Lifecycle,
			SupportsChat: tpl.Runtime.SupportsChat,
			ParentID:     parentID,
			OrgID:        orgID,
			Placement:    placement,
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

		// Human lifecycle authority tuples (Phase 1, org multi-tenancy). Always
		// write the owner tuple; when the record is org-scoped also write the org
		// tuple so org admins/members inherit view/manage via `org->...`. These
		// feed the agent `view`/`manage` permissions; nothing enforces them yet
		// (the authz flip from userId-equality is Phase 4) — writing them now is
		// additive and keeps the tuples present for when it lands.
		//
		// OrgID is the record's server-derived org (rec.OrgID == existing.OrgID),
		// never a body value. The owner tuple is skipped for a user-less global
		// agent so we never write a malformed `user:` (empty id) tuple — the same
		// guard the tenant-relation skip applies above for a tenant-less agent.
		// userId/orgId are normalized to a SpiceDB-safe object id here on the WRITE
		// side; agentAllowed applies the SAME spicedb.EncodeObjectID on the READ
		// (CheckPermission) side, and orgTuple does likewise for org membership —
		// so the owner/org/membership tuples and the Phase-4 checks agree even for
		// subjects carrying '. : @' (which ValidUserID admits but SpiceDB rejects
		// raw). For the common UUID-sub / DNS-label-org case the encoding is the
		// identity, so nothing changes for existing tuples.
		if rec.UserID != "" {
			ownerRes, ownerSub := "agent:"+agentID, "user:"+spicedb.EncodeObjectID(rec.UserID)
			if err := sdb.WriteRelationship(ctx, ownerRes, "owner", ownerSub); err != nil {
				log.Printf("spicedb owner-tuple write error for %s: %v", agentID, err)
			}
			tuples = append(tuples, relTuple{Resource: ownerRes, Relation: "owner", Subject: ownerSub})
		}
		if rec.OrgID != "" {
			orgRes, orgSub := "agent:"+agentID, "organization:"+spicedb.EncodeObjectID(rec.OrgID)
			if err := sdb.WriteRelationship(ctx, orgRes, "org", orgSub); err != nil {
				log.Printf("spicedb org-tuple write error for %s: %v", agentID, err)
			}
			tuples = append(tuples, relTuple{Resource: orgRes, Relation: "org", Subject: orgSub})
		}
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

	// Appending an event is an audit write, so it is authenticated with the SAME
	// dual model as the PATCH-status endpoint above: a control-plane caller (the
	// operator's reconciler/sweep and the orchestrator) may post to ANY agent;
	// otherwise the agent's OWN sidecar, proven by its SVID, may post to its own id
	// or its DIRECT parent's (the consent_denied breadcrumb the sidecar posts up to
	// its parent — the one legitimate cross-agent SVID write, discovered by
	// auditing every caller). An unauthenticated caller is rejected — this is what
	// stops any in-cluster process forging audit events. Under the AllowAll (demo/
	// kind) tier cpAuth passes for everyone, so the endpoint stays open and the
	// SVID branch is never reached — kind behaviour is unchanged.
	mux.HandleFunc("POST /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		if _, err := cpAuth.Authenticate(r.Context(), r); err != nil {
			id, verr := verifier.Verify(r.Context(), r)
			if verr != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if id.AgentID != agentID {
				// Not itself — allow only a child posting to its DIRECT parent. The
				// caller's own record names its parent; any other target is rejected.
				caller, err := s.GetAgent(r.Context(), id.AgentID)
				if storeErr(w, err) {
					return
				}
				if caller.ParentID != agentID {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
			}
		}
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

	// Reading an agent's event timeline is control-plane gated (its POST sibling
	// already is). The only legitimate reader is the orchestrator, whose
	// regClient carries the control-plane bearer via bearerTransport; the public
	// dashboard reaches this only through the orchestrator's own user-authorized,
	// ownership-scoped proxy. Gating the read keeps a direct in-cluster caller
	// from harvesting the timeline. Under CONTROL_PLANE_AUTH=none this is open,
	// so kind is unaffected.
	handleCP("GET /v1/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
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

	// PATCH /v1/agents/{id} — set an agent's lifecycle status. status=completed|failed
	// triggers IRREVERSIBLE SpiceDB cleanup (deletes the agent's relations + its
	// `enabled` tuple), so an unauthenticated cross-agent PATCH would destroy
	// another agent's authority. This has two legitimate callers, so it uses DUAL
	// AUTH: the operator's reconciler (Registry.Complete/Fail) authenticates as a
	// control-plane caller and may set ANY agent's status; the agent's own sidecar
	// (CIBA lifecycle: awaiting-consent/active/failed) presents its SVID and may
	// set only its OWN status.
	mux.HandleFunc("PATCH /v1/agents/", func(w http.ResponseWriter, r *http.Request) {
		agentID := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
		// Authorize: a control-plane caller (the operator) may set ANY agent's
		// status; otherwise the agent itself, proven by its SVID, may set only its
		// OWN status. An unauthenticated caller is rejected — this is what stops a
		// cross-agent completed/failed that would destroy another agent's
		// authority. Under the AllowAll (demo) tier cpAuth passes for everyone,
		// matching the open posture. The sidecar's `Authorization: Bearer <svid>`
		// fails the control-plane bearer compare and falls through to SVID verify —
		// no collision.
		if _, err := cpAuth.Authenticate(r.Context(), r); err != nil {
			id, verr := verifier.Verify(r.Context(), r)
			if verr != nil || id.AgentID != agentID {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
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
			//
			// Propagate a read error (5xx) rather than swallowing it: a failed
			// GetAgent/GetTemplate would leave rec.AgentType empty → the WRONG
			// (empty) resTypes → DeleteAgentRelationships would miss the agent's
			// tuples, leaving stale SpiceDB authority behind. The status is already
			// flipped, so a 5xx lets the caller retry the idempotent cleanup.
			rec, err := s.GetAgent(ctx, agentID)
			if storeErr(w, err) {
				return
			}
			var resTypes []string
			tpl, ok, err := s.GetTemplate(ctx, rec.AgentType)
			if storeErr(w, err) {
				return
			}
			if ok {
				resTypes = relationResourceTypes(tpl, rec.TenantID != "")
			}
			// Template relations (tenant/project/...) have "agent:<id>" as their
			// SUBJECT, so a subject-scoped delete reaches them.
			if err := sdb.DeleteAgentRelationships(ctx, agentID, resTypes); err != nil {
				log.Printf("spicedb cleanup error for %s: %v", agentID, err)
			}
			// Drop ONLY the `enabled` status tuple (a terminal agent isn't runnable),
			// exactly like revoke. The `owner` and `org` tuples are DELIBERATELY left
			// in place: they are the agent's HUMAN lifecycle authority (view/manage),
			// and a terminal agent still has a record a human must be able to view
			// and DISMISS. The record is never hard-deleted (dismiss is a soft flag;
			// nothing removes it from the store), so these tuples live with the
			// record — removing them here would 404 the owner's own dismiss/view of a
			// completed/failed/killed org-scoped agent (org authz checks manage/view =
			// owner + org->admin/view_agents against SpiceDB). This mirrors the schema
			// note that view/manage must stay checkable on a non-active agent.
			if err := sdb.DeleteRelationship(ctx, "agent:"+agentID, "enabled", "agent:"+agentID); err != nil {
				log.Printf("spicedb terminal enabled-tuple delete error for %s: %v", agentID, err)
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("POST /v1/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		agentID := r.PathValue("id")
		userId := r.URL.Query().Get("userId")
		// dismiss mutates the agent → manage authority.
		allowed, exists, err := agentAllowed(r.Context(), s, sdb, agentID, userId, actionManage)
		if storeErr(w, err) {
			return
		}
		if !exists || !allowed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
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
		id := r.PathValue("id")
		userId := r.URL.Query().Get("userId")
		// revoke mutates the agent (and its subtree) → manage authority.
		allowed, exists, err := agentAllowed(r.Context(), s, sdb, id, userId, actionManage)
		if storeErr(w, err) {
			return
		}
		if !exists || !allowed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		nodes, err := subtree(r.Context(), s, id)
		if storeErr(w, err) {
			return
		}
		if nodes == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		revoked := []string{}
		for _, id := range nodes {
			// A per-node backend error fails the whole cascade (5xx): better a loud,
			// retryable failure (revoke is idempotent) than a 200 that silently left
			// descendants un-revoked with live authority.
			ok, err := revokeNode(r.Context(), s, sdb, id)
			if storeErr(w, err) {
				return
			}
			if ok {
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
		id := r.PathValue("id")
		userId := r.URL.Query().Get("userId")
		// resume mutates the agent (and its subtree) → manage authority.
		allowed, exists, err := agentAllowed(r.Context(), s, sdb, id, userId, actionManage)
		if storeErr(w, err) {
			return
		}
		if !exists || !allowed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Suspension guard (signup-lifecycle Phase 4): while the agent's org is
		// SUSPENDED, refuse to resume — org disable revokes the org's agents AND
		// leaves owner tuples + dashboard sessions intact, so without this an owner
		// could immediately resume their agent and undo the suspension. Once the org
		// is re-enabled the (asymmetric) manual resume works normally. Fails CLOSED on
		// a store error.
		rec, err := s.GetAgent(r.Context(), id)
		if storeErr(w, err) {
			return
		}
		if disabled, derr := orgIsDisabled(r.Context(), s, rec.OrgID); storeErr(w, derr) {
			return
		} else if disabled {
			http.Error(w, "organization is disabled", http.StatusForbidden)
			return
		}
		nodes, err := subtree(r.Context(), s, id)
		if storeErr(w, err) {
			return
		}
		if nodes == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		resumed := []string{}
		for _, id := range nodes {
			ok, err := resumeNode(r.Context(), s, sdb, id)
			if storeErr(w, err) {
				return
			}
			if ok {
				resumed = append(resumed, id)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"resumed": resumed})
	})

	// Subtree is the read side of the cascade family: it returns the named agent
	// plus its entire descendant subtree (everything it spawned, transitively),
	// root first followed by a breadth-first walk — exactly the set a cascading
	// delete will operate on. It is read-only and storage-agnostic; the actual
	// cascading delete is owned by the orchestrator, which holds the k8s client
	// needed to tear pods down, and consumes this endpoint to learn what to delete.
	// Like its revoke/resume siblings it is a plain handler (no control-plane auth).
	mux.HandleFunc("GET /v1/agents/{id}/subtree", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		userId := r.URL.Query().Get("userId")
		// subtree is the read side of the DELETE cascade (a mutation the
		// orchestrator drives), so it takes manage authority per the spec matrix —
		// not view. (Since tenant-visibility Phase 2 view == manage == owner + org
		// admin, so a plain member — who owns neither view nor manage on a
		// non-owned org agent — cannot enumerate an admin's agent subtree.)
		allowed, exists, err := agentAllowed(r.Context(), s, sdb, id, userId, actionManage)
		if storeErr(w, err) {
			return
		}
		if !exists || !allowed {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		nodes, err := subtree(r.Context(), s, id)
		if storeErr(w, err) {
			return
		}
		if nodes == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"subtree": nodes})
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
				parentDepth, err := depth(ctx, s, parentID)
				if storeErr(w, err) {
					return
				}
				// The child would sit one level below the parent.
				if childDepth := parentDepth + 1; childDepth > max {
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

	// Record a fresh user consent for one spawn edge (called by Keycloak's CIBA
	// completion path when the user approves, or auto-renewed on auto-approval).
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
		// userId keys the consent edge — validate its charset before it reaches store keys.
		if !rejectInvalidUserID(w, req.UserID) {
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
	// requested scopes right now? Always HTTP 200; Keycloak's CIBA channel
	// provider calls this to decide auto-approve vs. prompt-the-user.
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
		// userId keys the consent edge + the open-request marker — validate its charset.
		if !rejectInvalidUserID(w, req.UserID) {
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
		// Read-time backstop: if this request is still pending only because the
		// approve-sweep's eventually-consistent index lagged, resolve it now
		// against the strongly-consistent grant so the poller sees "approved"
		// instead of waiting out CIBA expiry. No-op when genuinely pending or
		// already resolved (see backstopPendingConsentRequest).
		cr, err = backstopPendingConsentRequest(r.Context(), s, cr)
		if storeErr(w, err) {
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
		// ?detail=full returns the FULL template records (incl. disabled) for
		// the admin Agent Types view — a control-plane operation, so it is gated
		// by the same control-plane authenticator as template create/disable/
		// delete. The orchestrator's admin-only GET /v1/admin/templates forwards
		// here carrying the control-plane bearer. Without ?detail=full the route
		// stays public and returns the active type-name list (the spawn dropdown).
		if strings.EqualFold(r.URL.Query().Get("detail"), "full") {
			if _, err := cpAuth.Authenticate(r.Context(), r); err != nil {
				log.Printf("control-plane auth failed (detail=full): %v", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			tpls, err := s.ListTemplates(r.Context(), true)
			if storeErr(w, err) {
				return
			}
			// Org-scoped full view (Phase 6): with ?orgId= (the orchestrator's
			// org-admin Agent Types view) return ONLY that org's templates — an org
			// admin manages their org's catalog, not the global one nor another
			// org's. Without orgId (the realm-admin /v1/admin/templates view) return
			// everything. Note this is the "own org ONLY" scope (excludes global),
			// distinct from the spawn/list "global + own org" visibility below.
			if org := r.URL.Query().Get("orgId"); org != "" {
				tpls = filterTemplatesOwnOrg(tpls, org)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tpls)
			return
		}
		// ?detail=spawn returns the active set with just enough for the spawn UI
		// to decide tenant-ness: {agentType, requiresTenant}. Unlike detail=full
		// it stays PUBLIC (no control-plane auth) — a non-admin user spawning an
		// agent needs the flag, and it exposes nothing sensitive (active type
		// names + a boolean, no authz config, no disabled types). Phase 6: with
		// ?orgId= (the orchestrator forwards the caller's active org) the list is
		// global + that-org templates; without it, global only.
		if strings.EqualFold(r.URL.Query().Get("detail"), "spawn") {
			tpls, err := s.ListTemplates(r.Context(), false)
			if storeErr(w, err) {
				return
			}
			tpls = filterTemplatesVisible(tpls, r.URL.Query().Get("orgId"))
			out := make([]map[string]any, 0, len(tpls))
			for _, t := range tpls {
				out = append(out, map[string]any{
					"agentType":      t.AgentType,
					"requiresTenant": t.RequiresTenant,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
			return
		}
		// Bare list: active type names for the spawn dropdown. Phase 6: same
		// global + caller's-org visibility filter as detail=spawn (was
		// ListTemplateTypes; now ListTemplates(false) so OrgID is available to
		// filter on before projecting to names).
		tpls, err := s.ListTemplates(r.Context(), false)
		if storeErr(w, err) {
			return
		}
		tpls = filterTemplatesVisible(tpls, r.URL.Query().Get("orgId"))
		types := make([]string, 0, len(tpls))
		for _, t := range tpls {
			types = append(types, t.AgentType)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(types)
	})

	return rejectDuplicateUserID(mux)
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
	// id and agentId let a notifier (e.g. the mobile-gateway) deep-link the
	// prompt to the specific pending request/agent and fetch fresh detail over
	// an authenticated channel — the push itself carries no secrets.
	body, _ := json.Marshal(map[string]any{
		"type":           "consent_pending",
		"id":             cr.ID,
		"agentId":        cr.AgentID,
		"user":           cr.UserID,
		"parentType":     cr.ParentType,
		"childType":      cr.ChildType,
		"scopes":         cr.Scopes,
		"bindingMessage": cr.BindingMessage,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		log.Printf("consent notifier webhook build failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// The registry→notifier hop is a control-plane call: present the same shared
	// secret the registry's own callers use, so the notifier can reject anyone
	// who isn't the platform. Empty in the local/demo tier (auth=none).
	if token := os.Getenv("CONTROL_PLANE_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
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
	// chosen, its AgentID derivation MUST match the Keycloak attestation
	// verifier's, or minted tokens won't line up with registry records.
	var attestorDefault string
	switch a := getEnv("ATTESTOR", "spiffe"); a {
	case "spiffe":
		attestorDefault = "spiffe-svid"
	case "aws-sts":
		attestorDefault = "aws-sts"
	case "aws-stsweb":
		attestorDefault = "aws-stsweb"
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
	case "aws-stsweb":
		// Registration credential is an STS web-identity JWT (GetWebIdentityToken)
		// from a pod under EKS Pod Identity; AgentID comes from the cluster-attested
		// principal_tags.kubernetes-pod-name.
		issuer := getEnv("STSWEB_ISSUER", "")
		jwksURL := getEnv("STSWEB_JWKS_URL", "")
		if jwksURL == "" && issuer != "" {
			jwksURL = strings.TrimRight(issuer, "/") + "/.well-known/jwks.json"
		}
		if jwksURL == "" {
			log.Fatalf("STSWEB_ISSUER or STSWEB_JWKS_URL required when ATTESTOR=aws-stsweb")
		}
		verifier, err = registrant.NewStsWebVerifier(ctx, registrant.StsWebConfig{
			JWKSURL:        jwksURL,
			Issuer:         issuer,
			Audience:       getEnv("STSWEB_AUDIENCE", "spawnly"),
			Namespace:      getEnv("STSWEB_NAMESPACE", ""),
			ServiceAccount: getEnv("STSWEB_SERVICE_ACCOUNT", ""),
			ClusterARN:     getEnv("STSWEB_CLUSTER_ARN", ""),
		})
		if err != nil {
			log.Fatalf("stsweb verifier init: %v", err)
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

	// Control-plane auth is pluggable, mirroring registration auth: it gates the
	// consent-lifecycle endpoints, which only platform services call. Default
	// "none" runs them open (local demo); "shared-secret" requires a static
	// bearer; "oidc" validates a client-credentials access token against a
	// configured JWKS (any IdP), enforcing audience + scope.
	cpAuth := buildControlPlaneAuth(ctx)

	dir := buildOrgDirectory()

	s := buildStore(ctx)
	// Apply the active schema (default or SPICEDB_SCHEMA_PATH override) to whatever
	// backend was selected. The schema is boot-time config every Store keeps in
	// memory (registry.SchemaState), so this seeds the DynamoDB store — which
	// starts with none — and re-applies the same value to the in-memory store.
	s.SetSchema(activeSchema, schemaVersion, schemaSource)

	// Per-org verifier resolution (multi-tenancy Phase 9): wrap the configured
	// default registrant verifier so a consumer-hosted (remote) agent can
	// self-register with its org's AWS STS credential — resolved to the org that
	// owns the agent's record and bound to that org's active anchor (account +
	// role pattern). A non-STS credential (JWT-SVID/OIDC/mTLS) is passed straight
	// through, so platform SPIRE registration is bit-identical. The isolation
	// invariant (an STS credential verifies ONLY against its record's org anchor)
	// mirrors the Keycloak OrgAwareVerifier on the token-minting side.
	verifier = registrant.NewOrgAwareVerifier(verifier, s, nil)
	log.Printf("registry listening on :8080 (store=%s schema source=%s version=%s)",
		getEnv("STORE", "memory"), schemaSource, schemaVersion)
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           buildMux(s, sdb, verifier, cpAuth, dir),
		ReadHeaderTimeout: 10 * time.Second, // Slowloris defense; no risk to legit responses.
		ReadTimeout:       30 * time.Second, // request bodies are all small JSON.
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: responses are short JSON, but the orchestrator and
		// dashboard proxy long-lived endpoints, so we stay consistent and avoid
		// truncating any forwarded long response.
	}
	log.Fatal(srv.ListenAndServe())
}

// schemaSettableStore is a registry.Store that also accepts the boot-time
// SpiceDB schema. Every concrete store embeds registry.SchemaState, so both the
// in-memory and DynamoDB backends satisfy it; main() uses it so it can apply the
// active schema uniformly regardless of which backend STORE selected. SetSchema
// is intentionally NOT on registry.Store — it is config, not a persistence op.
type schemaSettableStore interface {
	registry.Store
	SetSchema(text, version, source string)
}

// buildStore selects the registry's persistence backend from the STORE env:
// "memory" (default) keeps the in-memory maps — the kind/local path, byte-for-byte
// unchanged; "dynamodb" uses the durable single-table DynamoDB store for the AWS
// deploy. It log.Fatals on a misconfiguration (matching buildControlPlaneAuth's
// fail-loud style) so a deploy that intends durable state never silently runs on
// ephemeral memory.
func buildStore(ctx context.Context) schemaSettableStore {
	switch v := getEnv("STORE", "memory"); v {
	case "memory":
		return newStore()
	case "dynamodb":
		table := getEnv("REGISTRY_DYNAMODB_TABLE", "")
		if table == "" {
			log.Fatalf("REGISTRY_DYNAMODB_TABLE required when STORE=dynamodb")
		}
		// Region + credentials come from the standard AWS chain (AWS_REGION,
		// container/IRSA/Pod-Identity credentials) — no static keys in config.
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			log.Fatalf("load AWS config for DynamoDB store: %v", err)
		}
		var opts []func(*dynamodb.Options)
		// DYNAMODB_ENDPOINT overrides the resolved endpoint for local runs
		// (dynamodb-local); unset in AWS, where the SDK resolves the real endpoint.
		if ep := getEnv("DYNAMODB_ENDPOINT", ""); ep != "" {
			opts = append(opts, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(ep) })
		}
		return dynamostore.New(cfg, table, opts...)
	default:
		log.Fatalf("unknown STORE %q", v)
		return nil // unreachable
	}
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

// buildOrgDirectory selects the org-membership directory backend. When
// ORG_ADMIN_CLIENT_SECRET is set (the bootstrap-generated org-admin client
// secret, deploy/keycloak + scripts/bootstrap.sh), it returns the Keycloak
// admin-REST adapter authenticating as the least-privilege org-admin
// service-account client. When unset — a bare `kubectl apply` with no org
// onboarding, or the pre-Phase-3 demo — it returns a disabled directory whose
// methods fail loudly, so the org endpoints 500 rather than silently corrupting
// half a dual-write. The secret is NEVER read from a file or baked into an image.
func buildOrgDirectory() orgdirectory.Directory {
	secret := getEnv("ORG_ADMIN_CLIENT_SECRET", "")
	if secret == "" {
		log.Printf("org directory: ORG_ADMIN_CLIENT_SECRET unset — org endpoints disabled")
		return disabledDirectory{}
	}
	base := getEnv("KEYCLOAK_URL", "http://keycloak:8080")
	realm := getEnv("KEYCLOAK_REALM", "spawnly")
	clientID := getEnv("ORG_ADMIN_CLIENT_ID", "org-admin")
	log.Printf("org directory: keycloak admin-REST (base=%s realm=%s client=%s)", base, realm, clientID)
	return keycloak.New(base, realm, clientID, secret, getEnv("DASHBOARD_CLIENT_ID", "dashboard"), nil)
}

// disabledDirectory is the org directory when no org-admin credential is
// configured. Every method fails, so an org endpoint returns 500 instead of
// half-completing a Keycloak/SpiceDB dual-write.
type disabledDirectory struct{}

var errOrgDirectoryDisabled = errors.New("org directory not configured (ORG_ADMIN_CLIENT_SECRET unset)")

func (disabledDirectory) CreateOrg(context.Context, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) DeleteOrg(context.Context, string) error { return errOrgDirectoryDisabled }
func (disabledDirectory) AddMember(context.Context, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) RemoveMember(context.Context, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) SetRole(context.Context, string, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) ListMembers(context.Context, string) ([]orgdirectory.Member, error) {
	return nil, errOrgDirectoryDisabled
}
func (disabledDirectory) ListUserOrgs(context.Context, string) ([]string, error) {
	return nil, errOrgDirectoryDisabled
}
func (disabledDirectory) RegisterOrgRedirectURIs(context.Context, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) RemoveOrgRedirectURIs(context.Context, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) ListUsers(context.Context) ([]orgdirectory.User, error) {
	return nil, errOrgDirectoryDisabled
}
func (disabledDirectory) DisableUser(context.Context, string, string) error {
	return errOrgDirectoryDisabled
}
func (disabledDirectory) EnableUser(context.Context, string, string) (bool, error) {
	return false, errOrgDirectoryDisabled
}
func (disabledDirectory) DeleteUser(context.Context, string) error {
	return errOrgDirectoryDisabled
}
