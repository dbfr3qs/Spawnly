package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

// remote-substrate AC 7 (lifecycle parity) regression cover.
//
// A substrate-placed agent's pod lives in the CONSUMER'S cluster, so its record
// stays `pending` — preregistered, not yet self-registered — for an unbounded
// time. Two independent defects made revoke unusable over exactly that window:
//
//  1. the owner/org SpiceDB tuples were written only at SELF-registration, so the
//     manage check on a preregistered record found nothing and fail-closed denied
//     (rendered 404, non-enumerable) — the live "revoke: 404";
//  2. revokeNode skipped any record that wasn't "active", so even an authorized
//     revoke of a pending agent was a silent no-op that reported success.
//
// These tests drive the real handlers so both are covered end to end.

// parityMux builds a registry mux with an in-memory store + SpiceDB mock and
// returns both so a test can assert on the record and the tuples.
func parityMux(t *testing.T) (http.Handler, *store, *spicedb.Mock) {
	t.Helper()
	s := newStore()
	s.putTemplate(workerTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/agent-sub"}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll(), testDir())
	return mux, s, sdb
}

// preregisterSubstrate preregisters a SUBSTRATE-PLACED, org-scoped record exactly
// as the orchestrator's spawn path does.
func preregisterSubstrate(t *testing.T, mux http.Handler, agentID, userID, orgID string) {
	t.Helper()
	body, _ := json.Marshal(registry.AgentRecord{
		AgentID: agentID, AgentType: "worker", UserID: userID, OrgID: orgID,
		Placement: registry.PlacementSubstrate,
	})
	req := httptest.NewRequest("POST", "/v1/agents/preregister", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("preregister: %d %s", rec.Code, rec.Body.String())
	}
}

func manageOp(t *testing.T, mux http.Handler, op, agentID, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/v1/agents/"+agentID+"/"+op+"?userId="+userID, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func statusOf(t *testing.T, s *store, id string) string {
	t.Helper()
	rec, err := s.GetAgent(t.Context(), id)
	if err != nil {
		t.Fatalf("get agent %s: %v", id, err)
	}
	return rec.Status
}

// The owner of a PENDING, substrate-placed, org-scoped agent can revoke it — and
// the revoke actually bites (record → revoked), which is what severs the next
// mint (a revoked record refuses self-registration). Resume restores it.
func TestPendingSubstrateAgent_OwnerCanRevokeAndResume(t *testing.T) {
	mux, s, _ := parityMux(t)
	seedOrg(t, s, "acme")
	preregisterSubstrate(t, mux, "agent-sub", "bob", "acme")

	if got := statusOf(t, s, "agent-sub"); got != "pending" {
		t.Fatalf("precondition: status = %q, want pending", got)
	}

	rec := manageOp(t, mux, "revoke", "agent-sub", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s (owner of a pending org agent must be authorized)", rec.Code, rec.Body.String())
	}
	var revoked struct {
		Revoked []string `json:"revoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &revoked); err != nil {
		t.Fatalf("decode revoke response: %v", err)
	}
	if len(revoked.Revoked) != 1 || revoked.Revoked[0] != "agent-sub" {
		t.Fatalf("revoked = %v, want [agent-sub] (a pending agent must actually be severed)", revoked.Revoked)
	}
	if got := statusOf(t, s, "agent-sub"); got != "revoked" {
		t.Fatalf("status after revoke = %q, want revoked", got)
	}

	if rec := manageOp(t, mux, "resume", "agent-sub", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("resume: %d %s", rec.Code, rec.Body.String())
	}
	if got := statusOf(t, s, "agent-sub"); got != "active" {
		t.Fatalf("status after resume = %q, want active", got)
	}
}

// Authority for a pending agent is exactly the same as for an active one: a
// non-owner in ANOTHER org is refused, and the refusal is 404 (non-enumerable),
// never 403. This is the isolation half of the fix — writing the tuples earlier
// must not widen who holds manage.
func TestPendingSubstrateAgent_NonOwnerAndCrossOrgRefused404(t *testing.T) {
	mux, s, sdb := parityMux(t)
	seedOrg(t, s, "acme")
	seedOrg(t, s, "globex")
	preregisterSubstrate(t, mux, "agent-sub", "bob", "acme")
	// mallory is an ADMIN of another org entirely — the strongest non-owner.
	if err := sdb.WriteRelationship(t.Context(), "organization:globex", "admin", "user:mallory"); err != nil {
		t.Fatalf("seed globex admin: %v", err)
	}

	for _, op := range []string{"revoke", "resume", "dismiss"} {
		for _, user := range []string{"mallory", "", "carol"} {
			if rec := manageOp(t, mux, op, "agent-sub", user); rec.Code != http.StatusNotFound {
				t.Errorf("%s as %q: got %d, want 404 (fail-closed, non-enumerable)", op, user, rec.Code)
			}
		}
	}
	if got := statusOf(t, s, "agent-sub"); got != "pending" {
		t.Fatalf("refused ops must not mutate the record; status = %q", got)
	}
}

// The org ADMIN of the agent's OWN org keeps manage over a pending agent (the
// org tuple written at preregistration is what carries this).
func TestPendingSubstrateAgent_OwnOrgAdminCanRevoke(t *testing.T) {
	mux, s, sdb := parityMux(t)
	seedOrg(t, s, "acme")
	preregisterSubstrate(t, mux, "agent-sub", "bob", "acme")
	if err := sdb.WriteRelationship(t.Context(), "organization:acme", "admin", "user:alice"); err != nil {
		t.Fatalf("seed acme admin: %v", err)
	}

	if rec := manageOp(t, mux, "revoke", "agent-sub", "alice"); rec.Code != http.StatusOK {
		t.Fatalf("org admin revoke: %d %s", rec.Code, rec.Body.String())
	}
	if got := statusOf(t, s, "agent-sub"); got != "revoked" {
		t.Fatalf("status after org-admin revoke = %q, want revoked", got)
	}
}

// revocableStatus is the whole behavioural rule: in-flight (pending) and settled
// (active) agents are revocable; terminal ones and an already-revoked one are not,
// so a cascade never resurrects or clobbers an agent that exited.
func TestRevocableStatus(t *testing.T) {
	for status, want := range map[string]bool{
		"pending":   true,
		"active":    true,
		"revoked":   false,
		"completed": false,
		"failed":    false,
		"killed":    false,
		"":          false,
	} {
		if got := revocableStatus(status); got != want {
			t.Errorf("revocableStatus(%q) = %v, want %v", status, got, want)
		}
	}
}

// A terminal record is untouched by revoke: the cascade reports nothing revoked
// and the status is preserved (revoke must never resurrect an exited agent).
func TestRevoke_TerminalRecordUntouched(t *testing.T) {
	mux, s, _ := parityMux(t)
	seedOrg(t, s, "acme")
	preregisterSubstrate(t, mux, "agent-sub", "bob", "acme")
	if _, err := s.UpdateAgentStatus(t.Context(), "agent-sub", "completed"); err != nil {
		t.Fatalf("set completed: %v", err)
	}

	rec := manageOp(t, mux, "revoke", "agent-sub", "bob")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body.String())
	}
	var revoked struct {
		Revoked []string `json:"revoked"`
	}
	json.Unmarshal(rec.Body.Bytes(), &revoked)
	if len(revoked.Revoked) != 0 {
		t.Errorf("revoked = %v, want [] for a completed agent", revoked.Revoked)
	}
	if got := statusOf(t, s, "agent-sub"); got != "completed" {
		t.Errorf("status = %q, want completed (preserved)", got)
	}
}

// An ORG-LESS preregistered agent keeps the pre-existing userId-equality path
// (no SpiceDB round-trip): its owner manages it, nobody else does.
func TestPendingOrgLessAgent_OwnerOnly(t *testing.T) {
	mux, s, _ := parityMux(t)
	preregisterSubstrate(t, mux, "agent-sub", "bob", "")

	if rec := manageOp(t, mux, "revoke", "agent-sub", "mallory"); rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner revoke of an org-less agent: got %d, want 404", rec.Code)
	}
	if rec := manageOp(t, mux, "revoke", "agent-sub", "bob"); rec.Code != http.StatusOK {
		t.Fatalf("owner revoke of an org-less agent: %d %s", rec.Code, rec.Body.String())
	}
	if got := statusOf(t, s, "agent-sub"); got != "revoked" {
		t.Fatalf("status = %q, want revoked", got)
	}
}
