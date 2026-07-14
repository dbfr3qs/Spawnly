package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spawnly/platform/internal/orgdirectory"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// agentStatus reads an agent record's current status from the store.
func agentStatus(t *testing.T, s *store, id string) string {
	t.Helper()
	return s.getAgent(id).Status
}

func TestListUsers(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	// Seed two users via the membership path (auto-creates them in the directory).
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "bob"})

	rec := doReq(t, mux, "GET", "/v1/users", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var users []struct {
		Username string `json:"username"`
		Enabled  bool   `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &users); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("got %d users, want 2 (alice,bob): %+v", len(users), users)
	}
	for _, u := range users {
		if !u.Enabled {
			t.Errorf("user %s should be enabled by default", u.Username)
		}
	}
}

func TestDisableUser_NoSuspend_LeavesAgentsRunning(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	dir.ensureUser("bob")
	s.registerAgent(registry.AgentRecord{AgentID: "a1", AgentType: "worker", UserID: "bob", Status: "active"})

	rec := doReq(t, mux, "POST", "/v1/users/bob/disable", map[string]any{"suspendAgents": false})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !dir.hasCall("DisableUser:bob|") {
		t.Fatalf("DisableUser not called; calls=%v", dir.calls)
	}
	if dir.logouts["bob"] != 1 {
		t.Errorf("expected sessions revoked once, got %d", dir.logouts["bob"])
	}
	if st := agentStatus(t, s, "a1"); st != "active" {
		t.Errorf("agent should keep running without suspendAgents, status=%q", st)
	}
}

func TestDisableUser_WithSuspend_RevokesOwnSubtreeOnly(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	dir.ensureUser("bob")
	// bob owns a1 -> a2 (child). carol owns c1 (must be untouched).
	s.registerAgent(registry.AgentRecord{AgentID: "a1", AgentType: "worker", UserID: "bob", Status: "active"})
	s.registerAgent(registry.AgentRecord{AgentID: "a2", AgentType: "worker", UserID: "bob", ParentID: "a1", Status: "active"})
	s.registerAgent(registry.AgentRecord{AgentID: "c1", AgentType: "worker", UserID: "carol", Status: "active"})

	rec := doReq(t, mux, "POST", "/v1/users/bob/disable", map[string]any{"suspendAgents": true})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if st := agentStatus(t, s, "a1"); st != "revoked" {
		t.Errorf("a1 should be revoked, got %q", st)
	}
	if st := agentStatus(t, s, "a2"); st != "revoked" {
		t.Errorf("a2 (bob's child) should be revoked, got %q", st)
	}
	if st := agentStatus(t, s, "c1"); st != "active" {
		t.Errorf("carol's agent must NOT be touched, got %q", st)
	}
}

func TestEnableUser_LeavesAgentsSuspended(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	dir.ensureUser("bob")
	s.registerAgent(registry.AgentRecord{AgentID: "a1", AgentType: "worker", UserID: "bob", Status: "active"})

	// Disable with suspend, then enable — agent stays revoked (asymmetric resume).
	doReq(t, mux, "POST", "/v1/users/bob/disable", map[string]any{"suspendAgents": true})
	if st := agentStatus(t, s, "a1"); st != "revoked" {
		t.Fatalf("precondition: a1 should be revoked, got %q", st)
	}
	rec := doReq(t, mux, "POST", "/v1/users/bob/enable", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("enable status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !dir.hasCall("EnableUser:bob|") {
		t.Fatalf("EnableUser not called; calls=%v", dir.calls)
	}
	if st := agentStatus(t, s, "a1"); st != "revoked" {
		t.Errorf("agent must STAY suspended after enable, got %q", st)
	}
}

func TestDeleteUser_AppliesDispositionThenDeletes(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	// acme = auto-revoke disposition; bob is a member owning an org agent.
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "PATCH", "/v1/orgs/acme", map[string]string{"removalDisposition": registry.RemovalAutoRevoke})
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "bob"})
	s.registerAgent(registry.AgentRecord{AgentID: "a1", AgentType: "worker", UserID: "bob", OrgID: "acme", Status: "active"})

	rec := doReq(t, mux, "DELETE", "/v1/users/bob", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	// Disposition applied (auto-revoke) → the org agent is revoked.
	if st := agentStatus(t, s, "a1"); st != "revoked" {
		t.Errorf("org agent should be revoked by auto-revoke disposition, got %q", st)
	}
	// Membership torn down and the identity deleted, in that order.
	if !dir.hasCall("RemoveMember:acme/bob") {
		t.Errorf("membership not removed; calls=%v", dir.calls)
	}
	if !dir.hasCall("DeleteUser:bob") {
		t.Errorf("DeleteUser not called; calls=%v", dir.calls)
	}
	if _, ok := dir.users["bob"]; ok {
		t.Error("bob should be gone from the directory")
	}
}

func TestDeleteUser_OrphanToAdminKeepsAgentRunning(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	// Default disposition (orphan-to-admin): the agent keeps running.
	doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"})
	doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "bob"})
	s.registerAgent(registry.AgentRecord{AgentID: "a1", AgentType: "worker", UserID: "bob", OrgID: "acme", Status: "active"})
	// Seed the owner tuple so the "dropped on delete" assertion below is real.
	if err := sdb.WriteRelationship(context.Background(), "agent:a1", "owner", "user:bob"); err != nil {
		t.Fatal(err)
	}

	if rec := doReq(t, mux, "DELETE", "/v1/users/bob", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if st := agentStatus(t, s, "a1"); st != "active" {
		t.Errorf("orphan-to-admin agent should keep running, got %q", st)
	}
	// Ex-member's owner tuple dropped (they lost all access).
	if sdb.HasRelationship("agent:a1", "owner", "user:bob") {
		t.Error("ex-member owner tuple must be dropped")
	}
}

// TestDeleteUser_SettlesOrgLessAgents proves delete revokes the user's ORG-LESS
// (global) agents — which authorize by userId equality (no owner tuple), so the
// per-org disposition never touches them — so they stop acting rather than being
// left running under a reusable username (blast-radius reduction; the equality
// residual only fully closes with immutable subject ids).
func TestDeleteUser_SettlesOrgLessAgents(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	dir.ensureUser("bob")
	// bob owns an org-less agent g1 -> g2 (child). carol's org-less agent untouched.
	s.registerAgent(registry.AgentRecord{AgentID: "g1", AgentType: "worker", UserID: "bob", Status: "active"})
	s.registerAgent(registry.AgentRecord{AgentID: "g2", AgentType: "worker", UserID: "bob", ParentID: "g1", Status: "active"})
	s.registerAgent(registry.AgentRecord{AgentID: "z1", AgentType: "worker", UserID: "carol", Status: "active"})

	if rec := doReq(t, mux, "DELETE", "/v1/users/bob", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if st := agentStatus(t, s, "g1"); st != "revoked" {
		t.Errorf("org-less agent g1 should be revoked on delete, got %q", st)
	}
	if st := agentStatus(t, s, "g2"); st != "revoked" {
		t.Errorf("org-less child g2 should be revoked on delete, got %q", st)
	}
	if st := agentStatus(t, s, "z1"); st != "active" {
		t.Errorf("another user's agent must NOT be touched, got %q", st)
	}
}

// TestDeleteUser_PurgesConsents proves delete revokes the user's CIBA consent
// grants so a re-registered username can't inherit their auto-approvals.
func TestDeleteUser_PurgesConsents(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	dir.ensureUser("bob")
	ctx := context.Background()
	if _, err := s.UpsertConsent(ctx, registry.ConsentRecord{
		ID: "c1", UserID: "bob", ParentType: "orchestrator", ChildType: "worker", Scopes: []string{"spawn"},
	}); err != nil {
		t.Fatal(err)
	}

	if rec := doReq(t, mux, "DELETE", "/v1/users/bob", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	got, err := s.ListConsents(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if !c.Revoked {
			t.Errorf("consent %s should be revoked after delete", c.ID)
		}
	}
}

func TestUserLifecycle_RootLockoutGuard(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	dir.ensureUser("root")

	// Default MASTER_ADMIN_USERNAME is "root" (getEnv fallback in buildMux).
	if rec := doReq(t, mux, "POST", "/v1/users/root/disable", map[string]any{"suspendAgents": true}); rec.Code != http.StatusForbidden {
		t.Fatalf("disable root = %d, want 403", rec.Code)
	}
	if rec := doReq(t, mux, "DELETE", "/v1/users/root", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("delete root = %d, want 403", rec.Code)
	}
	// The guard did not touch the directory.
	if dir.hasCallPrefix("DisableUser:root") || dir.hasCall("DeleteUser:root") {
		t.Fatalf("root guard must short-circuit before any directory call; calls=%v", dir.calls)
	}
	// Enabling root is harmless and allowed.
	if rec := doReq(t, mux, "POST", "/v1/users/root/enable", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("enable root = %d, want 204 (enable is not guarded)", rec.Code)
	}
}

func TestUserLifecycle_RootGuardHonoursEnvOverride(t *testing.T) {
	t.Setenv("MASTER_ADMIN_USERNAME", "superadmin")
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)

	// "root" is now an ordinary user (not the master admin) → disable allowed.
	dir.ensureUser("root")
	if rec := doReq(t, mux, "POST", "/v1/users/root/disable", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("disable ordinary 'root' under override = %d, want 204", rec.Code)
	}
	// The configured master admin is protected.
	if rec := doReq(t, mux, "DELETE", "/v1/users/superadmin", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("delete superadmin = %d, want 403", rec.Code)
	}
}

func TestUpdateUser_PartialEditPersists(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	u := dir.ensureUser("bob")
	u.firstName = "Bobby" // must survive an email-only edit

	rec := doReq(t, mux, "PATCH", "/v1/users/bob", map[string]any{"email": "bob@example.com"})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !dir.hasCall("UpdateUser:bob") {
		t.Fatalf("UpdateUser not called; calls=%v", dir.calls)
	}
	if u.email != "bob@example.com" {
		t.Errorf("email = %q, want bob@example.com", u.email)
	}
	if !u.emailVerified {
		t.Error("setting an email must pin emailVerified")
	}
	if u.firstName != "Bobby" {
		t.Errorf("firstName clobbered by an email-only edit: %q", u.firstName)
	}
	// The edit must NOT touch the lifecycle/enabled state.
	if !u.enabled {
		t.Error("UpdateUser must not change enabled")
	}
}

// TestUpdateUser_LeavesLifecycleUntouched proves an edit cannot lift a suspension:
// a disabled, org-suspended user stays disabled + suspended across a profile edit.
func TestUpdateUser_LeavesLifecycleUntouched(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	u := dir.ensureUser("bob")
	u.enabled = false
	u.disabledByOrg = []string{"acme"}
	u.disabledByAdmin = true

	if rec := doReq(t, mux, "PATCH", "/v1/users/bob", map[string]any{"firstName": "Bob"}); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if u.enabled {
		t.Error("edit must not re-enable a disabled user")
	}
	if len(u.disabledByOrg) != 1 || u.disabledByOrg[0] != "acme" {
		t.Errorf("disabledByOrg changed: %v", u.disabledByOrg)
	}
	if !u.disabledByAdmin {
		t.Error("disabledByAdmin ban must survive an edit")
	}
}

func TestUpdateUser_RootIsEditable(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	dir.ensureUser("root")
	// Unlike disable/delete, editing the master-admin's email is harmless → allowed.
	if rec := doReq(t, mux, "PATCH", "/v1/users/root", map[string]any{"email": "root@example.com"}); rec.Code != http.StatusNoContent {
		t.Fatalf("edit root = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if dir.users["root"].email != "root@example.com" {
		t.Errorf("root email not updated: %q", dir.users["root"].email)
	}
}

func TestUpdateUser_Validation(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	mux := orgMux(s, spicedb.NewMock(), dir)
	dir.ensureUser("bob")

	cases := []struct {
		name string
		path string
		body any
		want int
	}{
		{"invalid username", "/v1/users/bad%20user", map[string]any{"email": "a@b.io"}, http.StatusBadRequest},
		{"malformed email", "/v1/users/bob", map[string]any{"email": "not-an-email"}, http.StatusBadRequest},
		{"display-name email rejected", "/v1/users/bob", map[string]any{"email": "Bob <bob@x.io>"}, http.StatusBadRequest},
		{"empty email clears (allowed)", "/v1/users/bob", map[string]any{"email": ""}, http.StatusNoContent},
		{"names only ok", "/v1/users/bob", map[string]any{"firstName": "Bob", "lastName": "Jones"}, http.StatusNoContent},
		// An unknown field (e.g. a smuggled lifecycle field) is rejected, not ignored —
		// DisallowUnknownFields defends the "edit can't touch enabled/attributes" contract.
		{"unknown field rejected", "/v1/users/bob", map[string]any{"enabled": true}, http.StatusBadRequest},
		{"no fields to update", "/v1/users/bob", map[string]any{}, http.StatusBadRequest},
		{"over-long name", "/v1/users/bob", map[string]any{"firstName": strings.Repeat("x", 256)}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := doReq(t, mux, "PATCH", tc.path, tc.body); rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	// A missing/empty body is a 400 (a PATCH with nothing is a client error).
	req := httptest.NewRequest("PATCH", "/v1/users/bob", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty body = %d, want 400", rec.Code)
	}
}

// TestUpdateUser_EmailConflictIs409 proves a unique-email collision surfaced by
// the directory (ErrConflict) is mapped to 409, not an opaque 500.
func TestUpdateUser_EmailConflictIs409(t *testing.T) {
	s := newStore()
	dir := newFakeDirectory()
	dir.failUpdateUser = orgdirectory.ErrConflict
	mux := orgMux(s, spicedb.NewMock(), dir)
	dir.ensureUser("bob")
	rec := doReq(t, mux, "PATCH", "/v1/users/bob", map[string]any{"email": "taken@example.com"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestDisableUser_InvalidUsername400(t *testing.T) {
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	if rec := doReq(t, mux, "POST", "/v1/users/bad%20user/disable", nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid username = %d, want 400", rec.Code)
	}
}
