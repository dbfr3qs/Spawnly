// Package storetest is the shared behavioural conformance suite for the
// registry.Store contract. Every Store implementation (the in-memory store in
// package main, the DynamoDB store in package dynamostore) runs the exact same
// suite, so both are held to one definition of the contract — the persistence
// seam can be swapped without silently drifting semantics.
//
// The suite encodes the ACTUAL in-memory semantics the handlers depend on:
// zero-value GetAgent for an unknown id, dismissed/disabled list exclusions,
// stable consent ids across re-grants, openRequestKey dedup (agent-keyed and
// edge-keyed), user-scoped revoke/resolve, resolve idempotency, and the
// approve-sweep that auto-approves other covered pending requests on the edge.
//
// It deliberately does NOT cover SetSchema/Schema/ValidateTemplate: the active
// schema is boot-time config both stores keep in the shared registry.SchemaState
// (not persisted data), covered by the registry package's own schema tests.
//
// # Isolation
//
// Run calls newStore ONCE and shares that store across all sub-tests (matching
// the DynamoDB runner, which provisions a single dynamodb-local table per run).
// Sub-tests therefore use UNIQUE identifiers (unique agent ids, agent types,
// user ids) so they never collide, and assertions over global list endpoints
// (ListAgents, ListConsents(""), ListConsentRequests) check for
// presence/absence of the sub-test's own records rather than exact totals.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registry"
)

// Run executes the full conformance suite against the store returned by newStore.
func Run(t *testing.T, newStore func(t *testing.T) registry.Store) {
	s := newStore(t)
	ctx := context.Background()

	t.Run("Templates", func(t *testing.T) { testTemplates(t, ctx, s) })
	t.Run("Agents", func(t *testing.T) { testAgents(t, ctx, s) })
	t.Run("Events", func(t *testing.T) { testEvents(t, ctx, s) })
	t.Run("Consents", func(t *testing.T) { testConsents(t, ctx, s) })
	t.Run("ConsentExpiry", func(t *testing.T) { testConsentExpiry(t, ctx, s) })
	t.Run("ConsentRequestsDedupe", func(t *testing.T) { testConsentRequestDedupe(t, ctx, s) })
	t.Run("ConsentRequestResolve", func(t *testing.T) { testConsentResolve(t, ctx, s) })
	t.Run("ConsentRequestSweep", func(t *testing.T) { testConsentSweep(t, ctx, s) })
	t.Run("ApprovePendingConsentRequest", func(t *testing.T) { testApprovePending(t, ctx, s) })
}

// testApprovePending covers the status-only backstop primitive: it flips a
// pending request to approved WITHOUT creating a grant, is idempotent on an
// already-resolved request, and reports not-found for an unknown id.
func testApprovePending(t *testing.T, ctx context.Context, s registry.Store) {
	const (
		user = "st-ap-user"
		p    = "st-ap-parent"
		c    = "st-ap-child"
	)
	cr, _, err := s.CreateConsentRequest(ctx, registry.ConsentRequest{
		UserID: user, ParentType: p, ChildType: c, AgentID: "st-ap-agent", Scopes: []string{"x"},
	})
	if err != nil {
		t.Fatalf("CreateConsentRequest: %v", err)
	}

	got, ok, err := s.ApprovePendingConsentRequest(ctx, cr.ID)
	if err != nil || !ok {
		t.Fatalf("ApprovePendingConsentRequest: ok=%v err=%v", ok, err)
	}
	if got.Status != registry.ConsentApproved || got.ResolvedAt == nil {
		t.Errorf("flipped request = %+v, want approved+resolvedAt", got)
	}
	// Crucially, it must NOT have created a covering grant (that is the whole
	// point — the grant is a precondition the backstop never writes).
	if _, ok, _ := s.FindConsent(ctx, user, p, c); ok {
		t.Errorf("ApprovePendingConsentRequest wrongly created a ConsentRecord")
	}
	// Idempotent on the now-approved request.
	if again, ok, err := s.ApprovePendingConsentRequest(ctx, cr.ID); err != nil || !ok || again.Status != registry.ConsentApproved {
		t.Errorf("idempotent approve = %+v ok=%v err=%v", again, ok, err)
	}
	// Unknown id → not found.
	if _, ok, _ := s.ApprovePendingConsentRequest(ctx, "st-ap-nope"); ok {
		t.Errorf("ApprovePendingConsentRequest(unknown) reported found")
	}
}

// tpl builds a minimal valid template for the given type. The default in-memory
// schema (tenant/agent) accepts the tenant relation, so ValidateTemplate — which
// the create handler calls before PutTemplate — passes for it.
func tpl(agentType string) registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: agentType, Version: "1.0.0", Status: registry.TemplateStatusActive,
		Runtime: registry.RuntimeSpec{Image: "agent-" + agentType + ":latest"},
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	}
}

func mustPut(t *testing.T, ctx context.Context, s registry.Store, tp registry.AgentTemplate) {
	t.Helper()
	if err := s.PutTemplate(ctx, tp); err != nil {
		t.Fatalf("PutTemplate(%s): %v", tp.AgentType, err)
	}
}

func contains[T comparable](xs []T, want T) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func testTemplates(t *testing.T, ctx context.Context, s registry.Store) {
	active := tpl("st-tpl-active")
	disabled := tpl("st-tpl-disabled")
	disabled.Status = registry.TemplateStatusDisabled
	mustPut(t, ctx, s, active)
	mustPut(t, ctx, s, disabled)

	// Point get: found returns the stored body; unknown returns (zero, false).
	got, ok, err := s.GetTemplate(ctx, active.AgentType)
	if err != nil || !ok {
		t.Fatalf("GetTemplate(active): ok=%v err=%v", ok, err)
	}
	if got.Runtime.Image != active.Runtime.Image {
		t.Fatalf("GetTemplate image: got %q want %q", got.Runtime.Image, active.Runtime.Image)
	}
	if _, ok, err := s.GetTemplate(ctx, "st-tpl-nope"); ok || err != nil {
		t.Fatalf("GetTemplate(unknown): ok=%v err=%v, want false/nil", ok, err)
	}

	// ListTemplateTypes EXCLUDES disabled; ListTemplates(false/true) mirror that.
	names, err := s.ListTemplateTypes(ctx)
	if err != nil {
		t.Fatalf("ListTemplateTypes: %v", err)
	}
	if !contains(names, active.AgentType) {
		t.Errorf("ListTemplateTypes missing active %q: %v", active.AgentType, names)
	}
	if contains(names, disabled.AgentType) {
		t.Errorf("ListTemplateTypes leaked disabled %q: %v", disabled.AgentType, names)
	}
	full, err := s.ListTemplates(ctx, true)
	if err != nil {
		t.Fatalf("ListTemplates(true): %v", err)
	}
	if !hasTemplate(full, disabled.AgentType) {
		t.Errorf("ListTemplates(true) missing disabled %q", disabled.AgentType)
	}
	activeOnly, err := s.ListTemplates(ctx, false)
	if err != nil {
		t.Fatalf("ListTemplates(false): %v", err)
	}
	if hasTemplate(activeOnly, disabled.AgentType) {
		t.Errorf("ListTemplates(false) leaked disabled %q", disabled.AgentType)
	}

	// UpdateTemplateStatus: found updates + returns the new record; unknown → false.
	upd, found, err := s.UpdateTemplateStatus(ctx, active.AgentType, registry.TemplateStatusDisabled)
	if err != nil || !found {
		t.Fatalf("UpdateTemplateStatus: found=%v err=%v", found, err)
	}
	if upd.Status != registry.TemplateStatusDisabled {
		t.Errorf("UpdateTemplateStatus returned status %q, want disabled", upd.Status)
	}
	if _, found, _ := s.UpdateTemplateStatus(ctx, "st-tpl-nope", registry.TemplateStatusActive); found {
		t.Errorf("UpdateTemplateStatus(unknown) reported found")
	}

	// DeleteTemplate reports found-ness.
	del, err := s.DeleteTemplate(ctx, disabled.AgentType)
	if err != nil || !del {
		t.Fatalf("DeleteTemplate(existing): del=%v err=%v", del, err)
	}
	if del, _ := s.DeleteTemplate(ctx, disabled.AgentType); del {
		t.Errorf("DeleteTemplate(gone) reported found")
	}
	if del, _ := s.DeleteTemplate(ctx, "st-tpl-nope"); del {
		t.Errorf("DeleteTemplate(unknown) reported found")
	}
}

func hasTemplate(ts []registry.AgentTemplate, agentType string) bool {
	for _, t := range ts {
		if t.AgentType == agentType {
			return true
		}
	}
	return false
}

func testAgents(t *testing.T, ctx context.Context, s registry.Store) {
	// Unknown id → zero value, no error (the contract every handler leans on).
	zero, err := s.GetAgent(ctx, "st-ag-unknown")
	if err != nil {
		t.Fatalf("GetAgent(unknown): %v", err)
	}
	if zero.AgentID != "" {
		t.Fatalf("GetAgent(unknown) = %+v, want zero value", zero)
	}

	parent := registry.AgentRecord{AgentID: "st-ag-parent", AgentType: "worker", UserID: "st-user-a", Status: "active"}
	child := registry.AgentRecord{AgentID: "st-ag-child", AgentType: "worker", UserID: "st-user-a", Status: "active", ParentID: parent.AgentID}
	dismissed := registry.AgentRecord{AgentID: "st-ag-dismissed", AgentType: "worker", UserID: "st-user-a", Status: "active"}
	for _, r := range []registry.AgentRecord{parent, child, dismissed} {
		if err := s.RegisterAgent(ctx, r); err != nil {
			t.Fatalf("RegisterAgent(%s): %v", r.AgentID, err)
		}
	}

	gotParent, err := s.GetAgent(ctx, parent.AgentID)
	if err != nil || gotParent.AgentID != parent.AgentID || gotParent.Status != "active" {
		t.Fatalf("GetAgent(parent) = %+v err=%v", gotParent, err)
	}

	// ListChildren returns direct children only.
	kids, err := s.ListChildren(ctx, parent.AgentID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(kids) != 1 || kids[0].AgentID != child.AgentID {
		t.Fatalf("ListChildren(parent) = %+v, want [child]", kids)
	}
	if kids, _ := s.ListChildren(ctx, "st-ag-child"); len(kids) != 0 {
		t.Errorf("ListChildren(leaf) = %+v, want empty", kids)
	}

	// UpdateAgentStatus: found updates; unknown → false.
	if ok, err := s.UpdateAgentStatus(ctx, parent.AgentID, "completed"); err != nil || !ok {
		t.Fatalf("UpdateAgentStatus(parent): ok=%v err=%v", ok, err)
	}
	if got, _ := s.GetAgent(ctx, parent.AgentID); got.Status != "completed" {
		t.Errorf("status after update = %q, want completed", got.Status)
	}
	if ok, _ := s.UpdateAgentStatus(ctx, "st-ag-unknown", "failed"); ok {
		t.Errorf("UpdateAgentStatus(unknown) reported found")
	}

	// DismissAgent: found flips dismissed and removes it from ListAgents; unknown → false.
	if ok, err := s.DismissAgent(ctx, dismissed.AgentID); err != nil || !ok {
		t.Fatalf("DismissAgent: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.DismissAgent(ctx, "st-ag-unknown"); ok {
		t.Errorf("DismissAgent(unknown) reported found")
	}

	all, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if !hasAgent(all, parent.AgentID) || !hasAgent(all, child.AgentID) {
		t.Errorf("ListAgents missing a live agent: %v", agentIDs(all))
	}
	if hasAgent(all, dismissed.AgentID) {
		t.Errorf("ListAgents leaked dismissed agent %q", dismissed.AgentID)
	}
}

func hasAgent(rs []registry.AgentRecord, id string) bool {
	for _, r := range rs {
		if r.AgentID == id {
			return true
		}
	}
	return false
}

func agentIDs(rs []registry.AgentRecord) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.AgentID
	}
	return out
}

func testEvents(t *testing.T, ctx context.Context, s registry.Store) {
	const agentID = "st-ev-agent"
	// A fresh agent has no events.
	if evs, err := s.GetEvents(ctx, agentID); err != nil || len(evs) != 0 {
		t.Fatalf("GetEvents(empty) = %v err=%v", evs, err)
	}

	var wantOrder []string
	for _, typ := range []string{"first", "second", "third"} {
		stored, err := s.AppendEvent(ctx, agentID, events.Event{Source: events.SourceRegistry, Type: typ})
		if err != nil {
			t.Fatalf("AppendEvent(%s): %v", typ, err)
		}
		// AppendEvent stamps and returns the stored copy.
		if stored.ID == "" {
			t.Errorf("AppendEvent(%s) returned empty ID", typ)
		}
		if stored.Timestamp.IsZero() {
			t.Errorf("AppendEvent(%s) returned zero Timestamp", typ)
		}
		if stored.Type != typ {
			t.Errorf("AppendEvent returned Type %q, want %q", stored.Type, typ)
		}
		wantOrder = append(wantOrder, typ)
		// Space appends so the nanosecond SK is distinct — same-nanosecond append
		// order is not a Store guarantee (the DynamoDB SK disambiguates with a
		// random suffix), so the suite doesn't assert it.
		time.Sleep(time.Millisecond)
	}

	got, err := s.GetEvents(ctx, agentID)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("GetEvents len = %d, want %d", len(got), len(wantOrder))
	}
	seenID := map[string]bool{}
	for i, e := range got {
		if e.Type != wantOrder[i] {
			t.Errorf("GetEvents[%d].Type = %q, want %q (append order not preserved)", i, e.Type, wantOrder[i])
		}
		// Event ids must be unique — the dashboard's incremental renderer keys
		// off them, so a collision would hide or duplicate a row.
		if seenID[e.ID] {
			t.Errorf("GetEvents[%d].ID = %q is a duplicate; ids must be unique", i, e.ID)
		}
		seenID[e.ID] = true
	}
}

func testConsents(t *testing.T, ctx context.Context, s registry.Store) {
	const (
		userA = "st-con-userA"
		userB = "st-con-userB"
		pType = "st-con-parent"
		cType = "st-con-child"
	)
	now := time.Now()

	// Upsert assigns a non-empty id; find returns it.
	first, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		UserID: userA, ParentType: pType, ChildType: cType, Scopes: []string{"a"}, GrantedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertConsent(first): %v", err)
	}
	if first.ID == "" {
		t.Fatalf("UpsertConsent(first) returned empty id")
	}

	// Re-grant of the SAME edge keeps the SAME id, updates the scopes.
	second, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		UserID: userA, ParentType: pType, ChildType: cType, Scopes: []string{"a", "b"}, GrantedAt: now,
	})
	if err != nil {
		t.Fatalf("UpsertConsent(second): %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("re-grant id = %q, want stable %q", second.ID, first.ID)
	}

	found, ok, err := s.FindConsent(ctx, userA, pType, cType)
	if err != nil || !ok {
		t.Fatalf("FindConsent: ok=%v err=%v", ok, err)
	}
	if len(found.Scopes) != 2 {
		t.Errorf("FindConsent scopes = %v, want the re-granted [a b]", found.Scopes)
	}
	if _, ok, _ := s.FindConsent(ctx, userA, pType, "st-con-other"); ok {
		t.Errorf("FindConsent(unknown edge) reported found")
	}

	// ExpiresAt round-trips, and a re-grant without an expiry CLEARS a prior one
	// (a fresh grant replaces the whole record — the in-memory semantics the
	// DynamoDB store mirrors with SET/REMOVE expiresAt).
	const cExp = "st-con-child-exp"
	exp := now.Add(time.Hour)
	if _, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		UserID: userA, ParentType: pType, ChildType: cExp, Scopes: []string{"a"}, GrantedAt: now, ExpiresAt: &exp,
	}); err != nil {
		t.Fatalf("UpsertConsent(expiry): %v", err)
	}
	withExp, ok, _ := s.FindConsent(ctx, userA, pType, cExp)
	if !ok || withExp.ExpiresAt == nil {
		t.Fatalf("FindConsent(expiry) = %+v ok=%v, want a non-nil ExpiresAt", withExp, ok)
	}
	if d := withExp.ExpiresAt.Sub(exp); d < -time.Second || d > time.Second {
		t.Errorf("round-tripped ExpiresAt drifted by %v", d)
	}
	if _, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		UserID: userA, ParentType: pType, ChildType: cExp, Scopes: []string{"a"}, GrantedAt: now, // ExpiresAt nil
	}); err != nil {
		t.Fatalf("UpsertConsent(clear expiry): %v", err)
	}
	if cleared, _, _ := s.FindConsent(ctx, userA, pType, cExp); cleared.ExpiresAt != nil {
		t.Errorf("re-grant without expiry left ExpiresAt = %v, want nil (cleared)", cleared.ExpiresAt)
	}

	// A second user's grant on the same types is a distinct record.
	if _, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		UserID: userB, ParentType: pType, ChildType: cType, Scopes: []string{"z"}, GrantedAt: now,
	}); err != nil {
		t.Fatalf("UpsertConsent(userB): %v", err)
	}

	// ListConsents(user) is scoped to that user exactly (userA now has two edges:
	// cType and cExp), and never leaks another user's record.
	aList, err := s.ListConsents(ctx, userA)
	if err != nil {
		t.Fatalf("ListConsents(userA): %v", err)
	}
	if len(aList) != 2 {
		t.Errorf("ListConsents(userA) len = %d, want 2 (%+v)", len(aList), aList)
	}
	for _, rec := range aList {
		if rec.UserID != userA {
			t.Errorf("ListConsents(userA) leaked another user's record: %+v", rec)
		}
	}
	// ListConsents("") includes both (presence check — the store is shared).
	allList, err := s.ListConsents(ctx, "")
	if err != nil {
		t.Fatalf("ListConsents(all): %v", err)
	}
	if !hasConsentUser(allList, userA) || !hasConsentUser(allList, userB) {
		t.Errorf("ListConsents(all) missing a user: %+v", allList)
	}

	// Revoke is user-scoped: userB cannot revoke userA's grant...
	if ok, err := s.RevokeConsent(ctx, first.ID, userB); err != nil || ok {
		t.Errorf("RevokeConsent(cross-user) = %v err=%v, want false", ok, err)
	}
	if rec, _, _ := s.FindConsent(ctx, userA, pType, cType); rec.Revoked {
		t.Errorf("cross-user revoke wrongly revoked userA's grant")
	}
	// ...but the owner (or an empty scope) can.
	if ok, err := s.RevokeConsent(ctx, first.ID, userA); err != nil || !ok {
		t.Fatalf("RevokeConsent(owner) = %v err=%v, want true", ok, err)
	}
	if rec, _, _ := s.FindConsent(ctx, userA, pType, cType); !rec.Revoked {
		t.Errorf("owner revoke did not set Revoked")
	}
	if ok, _ := s.RevokeConsent(ctx, "st-con-nope", ""); ok {
		t.Errorf("RevokeConsent(unknown id) reported found")
	}
}

func hasConsentUser(cs []registry.ConsentRecord, user string) bool {
	for _, c := range cs {
		if c.UserID == user {
			return true
		}
	}
	return false
}

func testConsentExpiry(t *testing.T, ctx context.Context, s registry.Store) {
	const (
		parentTTL   = "st-exp-parent"
		parentNoTTL = "st-exp-parent-nottl"
		child       = "st-exp-child"
	)
	// A template whose child policy sets consentTTL drives the expiry.
	tp := tpl(parentTTL)
	tp.Delegation = registry.DelegationPolicy{
		AllowedChildTypes: []string{child},
		ChildPolicies:     map[string]registry.ChildSpawnPolicy{child: {RequireUserConsent: true, ConsentTTL: "1h"}},
	}
	mustPut(t, ctx, s, tp)

	// A template with an empty TTL yields no expiry.
	tp2 := tpl(parentNoTTL)
	tp2.Delegation = registry.DelegationPolicy{
		AllowedChildTypes: []string{child},
		ChildPolicies:     map[string]registry.ChildSpawnPolicy{child: {RequireUserConsent: true}},
	}
	mustPut(t, ctx, s, tp2)

	from := time.Now()
	exp, err := s.ConsentExpiry(ctx, parentTTL, child, from)
	if err != nil {
		t.Fatalf("ConsentExpiry(ttl): %v", err)
	}
	if exp == nil {
		t.Fatalf("ConsentExpiry(ttl) = nil, want ~from+1h")
	}
	if d := exp.Sub(from); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("ConsentExpiry delta = %v, want ~1h", d)
	}

	if exp, err := s.ConsentExpiry(ctx, parentNoTTL, child, from); err != nil || exp != nil {
		t.Errorf("ConsentExpiry(empty TTL) = %v err=%v, want nil", exp, err)
	}
	// Unknown parent template → nil (no expiry).
	if exp, err := s.ConsentExpiry(ctx, "st-exp-nope", child, from); err != nil || exp != nil {
		t.Errorf("ConsentExpiry(unknown parent) = %v err=%v, want nil", exp, err)
	}
}

func testConsentRequestDedupe(t *testing.T, ctx context.Context, s registry.Store) {
	// Edge-keyed dedup: two creates on the same edge with no agentId collapse to
	// one open request (created=false the second time, same id).
	edgeReq := registry.ConsentRequest{UserID: "st-cr-userE", ParentType: "st-cr-p", ChildType: "st-cr-c", Scopes: []string{"x"}}
	e1, created1, err := s.CreateConsentRequest(ctx, edgeReq)
	if err != nil || !created1 {
		t.Fatalf("CreateConsentRequest(edge #1): created=%v err=%v", created1, err)
	}
	if e1.Status != registry.ConsentPending || e1.ID == "" || e1.CreatedAt.IsZero() {
		t.Fatalf("edge request not initialised: %+v", e1)
	}
	e2, created2, err := s.CreateConsentRequest(ctx, edgeReq)
	if err != nil {
		t.Fatalf("CreateConsentRequest(edge #2): %v", err)
	}
	if created2 {
		t.Errorf("second edge create reported created=true, want dedupe")
	}
	if e2.ID != e1.ID {
		t.Errorf("edge dedupe id = %q, want %q", e2.ID, e1.ID)
	}

	// Agent-keyed dedup: two DIFFERENT agents on the same edge get their OWN
	// requests; the SAME agent dedupes.
	base := registry.ConsentRequest{UserID: "st-cr-userA", ParentType: "st-cr-p2", ChildType: "st-cr-c2", Scopes: []string{"x"}}
	reqA := base
	reqA.AgentID = "st-cr-agent-a"
	reqB := base
	reqB.AgentID = "st-cr-agent-b"

	a1, ca1, _ := s.CreateConsentRequest(ctx, reqA)
	b1, cb1, _ := s.CreateConsentRequest(ctx, reqB)
	if !ca1 || !cb1 {
		t.Fatalf("distinct agents should both create: a=%v b=%v", ca1, cb1)
	}
	if a1.ID == b1.ID {
		t.Errorf("two agents on one edge shared a request id %q", a1.ID)
	}
	if a1.AgentID != reqA.AgentID || b1.AgentID != reqB.AgentID {
		t.Errorf("agentId not preserved: %q %q", a1.AgentID, b1.AgentID)
	}
	a2, ca2, _ := s.CreateConsentRequest(ctx, reqA)
	if ca2 || a2.ID != a1.ID {
		t.Errorf("same agent re-create: created=%v id=%q, want dedupe to %q", ca2, a2.ID, a1.ID)
	}

	// CreateApprovedConsentRequest stores an already-resolved approved request.
	appr, err := s.CreateApprovedConsentRequest(ctx, registry.ConsentRequest{
		UserID: "st-cr-userAppr", ParentType: "st-cr-p3", ChildType: "st-cr-c3", Scopes: []string{"x"},
	})
	if err != nil {
		t.Fatalf("CreateApprovedConsentRequest: %v", err)
	}
	if appr.Status != registry.ConsentApproved || appr.ResolvedAt == nil || appr.ID == "" {
		t.Fatalf("approved request not initialised: %+v", appr)
	}

	// Get + list (scoped) round-trip.
	got, ok, err := s.GetConsentRequest(ctx, a1.ID)
	if err != nil || !ok || got.ID != a1.ID {
		t.Fatalf("GetConsentRequest(a1): got=%+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := s.GetConsentRequest(ctx, "st-cr-nope"); ok {
		t.Errorf("GetConsentRequest(unknown) reported found")
	}
	scoped, err := s.ListConsentRequests(ctx, "st-cr-userA", "")
	if err != nil {
		t.Fatalf("ListConsentRequests(userA): %v", err)
	}
	if len(scoped) != 2 { // reqA + reqB, both under st-cr-userA
		t.Errorf("ListConsentRequests(userA) len = %d, want 2 (%+v)", len(scoped), reqIDs(scoped))
	}
	pendingScoped, err := s.ListConsentRequests(ctx, "st-cr-userA", "pending")
	if err != nil {
		t.Fatalf("ListConsentRequests(userA,pending): %v", err)
	}
	if len(pendingScoped) != 2 {
		t.Errorf("ListConsentRequests(userA,pending) len = %d, want 2", len(pendingScoped))
	}
}

func reqIDs(rs []registry.ConsentRequest) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

func testConsentResolve(t *testing.T, ctx context.Context, s registry.Store) {
	mk := func(user, p, c, agent string, scopes []string) registry.ConsentRequest {
		t.Helper()
		cr, _, err := s.CreateConsentRequest(ctx, registry.ConsentRequest{
			UserID: user, ParentType: p, ChildType: c, AgentID: agent, Scopes: scopes,
		})
		if err != nil {
			t.Fatalf("CreateConsentRequest: %v", err)
		}
		return cr
	}

	// Deny leaves no covering ConsentRecord.
	denyReq := mk("st-rs-userDeny", "st-rs-pD", "st-rs-cD", "", []string{"x"})
	denied, ok, err := s.ResolveConsentRequest(ctx, denyReq.ID, "", false, nil)
	if err != nil || !ok {
		t.Fatalf("resolve deny: ok=%v err=%v", ok, err)
	}
	if denied.Status != registry.ConsentDenied || denied.ResolvedAt == nil {
		t.Errorf("denied request = %+v, want denied+resolvedAt", denied)
	}
	if _, ok, _ := s.FindConsent(ctx, "st-rs-userDeny", "st-rs-pD", "st-rs-cD"); ok {
		t.Errorf("deny created a ConsentRecord")
	}

	// Approve upserts a covering grant.
	apprReq := mk("st-rs-userAppr", "st-rs-pA", "st-rs-cA", "", []string{"x", "y"})
	approved, ok, err := s.ResolveConsentRequest(ctx, apprReq.ID, "", true, nil)
	if err != nil || !ok {
		t.Fatalf("resolve approve: ok=%v err=%v", ok, err)
	}
	if approved.Status != registry.ConsentApproved || approved.ResolvedAt == nil {
		t.Errorf("approved request = %+v, want approved+resolvedAt", approved)
	}
	rec, ok, _ := s.FindConsent(ctx, "st-rs-userAppr", "st-rs-pA", "st-rs-cA")
	if !ok {
		t.Fatalf("approve did not upsert a ConsentRecord")
	}
	if d := registry.EvaluateConsent(rec, []string{"x", "y"}, time.Now()); !d.Granted {
		t.Errorf("upserted grant does not cover the approved scopes: %+v", d)
	}

	// Idempotency: resolving an already-approved request returns it unchanged.
	again, ok, err := s.ResolveConsentRequest(ctx, apprReq.ID, "", true, nil)
	if err != nil || !ok {
		t.Fatalf("resolve idempotent: ok=%v err=%v", ok, err)
	}
	if again.Status != registry.ConsentApproved {
		t.Errorf("idempotent resolve status = %q, want approved", again.Status)
	}

	// User-scoping: a different user cannot resolve, and the request stays pending.
	scopedReq := mk("st-rs-userOwner", "st-rs-pS", "st-rs-cS", "", []string{"x"})
	if _, ok, _ := s.ResolveConsentRequest(ctx, scopedReq.ID, "st-rs-attacker", true, nil); ok {
		t.Errorf("cross-user resolve reported ok=true")
	}
	if got, _, _ := s.GetConsentRequest(ctx, scopedReq.ID); got.Status != registry.ConsentPending {
		t.Errorf("cross-user resolve changed status to %q, want still pending", got.Status)
	}
	if _, ok, _ := s.FindConsent(ctx, "st-rs-userOwner", "st-rs-pS", "st-rs-cS"); ok {
		t.Errorf("cross-user resolve wrongly created a grant")
	}
	// The rightful owner can still resolve it.
	if _, ok, err := s.ResolveConsentRequest(ctx, scopedReq.ID, "st-rs-userOwner", true, nil); err != nil || !ok {
		t.Errorf("owner resolve after cross-user attempt: ok=%v err=%v", ok, err)
	}

	// Resolving an unknown id is not-found.
	if _, ok, _ := s.ResolveConsentRequest(ctx, "st-rs-nope", "", true, nil); ok {
		t.Errorf("resolve(unknown) reported ok=true")
	}
}

func testConsentSweep(t *testing.T, ctx context.Context, s registry.Store) {
	const (
		user = "st-sw-user"
		p    = "st-sw-parent"
		c    = "st-sw-child"
	)
	mk := func(agent string, scopes []string) registry.ConsentRequest {
		t.Helper()
		cr, _, err := s.CreateConsentRequest(ctx, registry.ConsentRequest{
			UserID: user, ParentType: p, ChildType: c, AgentID: agent, Scopes: scopes,
		})
		if err != nil {
			t.Fatalf("CreateConsentRequest(%s): %v", agent, err)
		}
		return cr
	}

	// Three agents wait on the SAME edge. covered gets scope [x], alsoCovered [x],
	// wider [x,z] (z will not be granted).
	covered := mk("st-sw-a", []string{"x"})
	alsoCovered := mk("st-sw-b", []string{"x"})
	wider := mk("st-sw-c", []string{"x", "z"})

	// Approve one of the [x] requests with scope [x]: the grant covers [x].
	if _, ok, err := s.ResolveConsentRequest(ctx, covered.ID, "", true, []string{"x"}); err != nil || !ok {
		t.Fatalf("approve covered: ok=%v err=%v", ok, err)
	}

	// The sibling [x] request is swept to approved by the shared-edge grant.
	if got, _, _ := s.GetConsentRequest(ctx, alsoCovered.ID); got.Status != registry.ConsentApproved {
		t.Errorf("sibling [x] request = %q, want swept to approved", got.Status)
	}
	// The wider [x,z] request stays pending — z is not covered by the grant.
	if got, _, _ := s.GetConsentRequest(ctx, wider.ID); got.Status != registry.ConsentPending {
		t.Errorf("wider [x,z] request = %q, want still pending (z uncovered)", got.Status)
	}
}
