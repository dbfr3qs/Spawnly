package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

// seedOrgLifecycle builds org "acme" (admin alice) with members bob, carol, and
// root (the master admin), plus a running two-node agent tree in acme and one
// agent in a different org (globex) that org actions must never touch. carol is
// then independently disabled at the USER level (byOrg="") so the no-clobber
// behaviour can be proven. Returns the mux + wired store/dir/sdb.
func seedOrgLifecycle(t *testing.T) (http.Handler, *store, *fakeDirectory, *spicedb.Mock) {
	t.Helper()
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"}); rec.Code != http.StatusCreated {
		t.Fatalf("create acme: %d %s", rec.Code, rec.Body.String())
	}
	for _, u := range []string{"bob", "carol", "root"} {
		if rec := doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": u}); rec.Code != http.StatusCreated {
			t.Fatalf("add member %s: %d %s", u, rec.Code, rec.Body.String())
		}
	}
	// A different org with its own member + agent — the isolation control.
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "globex", "adminUser": "dave"}); rec.Code != http.StatusCreated {
		t.Fatalf("create globex: %d %s", rec.Code, rec.Body.String())
	}

	for _, a := range []registry.AgentRecord{
		{AgentID: "ar", AgentType: "worker", UserID: "bob", OrgID: "acme", Status: "active"},
		{AgentID: "ac", AgentType: "worker", UserID: "bob", OrgID: "acme", Status: "active", ParentID: "ar"},
		{AgentID: "gx", AgentType: "worker", UserID: "dave", OrgID: "globex", Status: "active"},
	} {
		s.registerAgent(a)
		sdb.WriteRelationship(t.Context(), "agent:"+a.AgentID, "enabled", "agent:"+a.AgentID)
	}

	// carol is independently disabled by an admin (byOrg=""), NOT by the org.
	if rec := doReq(t, mux, "POST", "/v1/users/carol/disable", map[string]any{}); rec.Code != http.StatusNoContent {
		t.Fatalf("independently disable carol: %d %s", rec.Code, rec.Body.String())
	}
	return mux, s, dir, sdb
}

func TestOrgDisable_LocksMembersSkipsMasterAdmin_TagsBannedToo_SuspendsAgents(t *testing.T) {
	mux, s, dir, _ := seedOrgLifecycle(t)

	rec := doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}

	// Org record flagged.
	if org, _, _ := s.GetOrg(t.Context(), "acme"); !org.Disabled {
		t.Errorf("org acme should be flagged Disabled")
	}
	// Currently-enabled members (alice, bob) disabled + tagged with the org.
	for _, u := range []string{"alice", "bob"} {
		st := dir.users[u]
		if st == nil || st.enabled {
			t.Errorf("member %s should be disabled", u)
		}
		if st != nil && !st.hasOrg("acme") {
			t.Errorf("member %s disabledByOrg = %v, want it to include acme", u, st.disabledByOrg)
		}
		if dir.logouts[u] == 0 {
			t.Errorf("member %s sessions should have been revoked", u)
		}
	}
	// The master admin (root) is never touched.
	if st := dir.users["root"]; st == nil || !st.enabled || len(st.disabledByOrg) != 0 {
		t.Errorf("master admin root must be left enabled + untagged, got %+v", st)
	}
	if dir.hasCallPrefix("DisableUser:root") {
		t.Errorf("DisableUser must never be called on the master admin")
	}
	// The independently admin-banned member (carol) is ALSO org-tagged now: the
	// multi-valued set + the SEPARATE ban marker mean recording this org's suspension
	// never clobbers the admin ban — and recording it is REQUIRED so a later enable of
	// a different org can't wrongly resurrect her. She stays disabled.
	if st := dir.users["carol"]; st == nil || st.enabled || !st.disabledByAdmin || !st.hasOrg("acme") {
		t.Errorf("admin-banned carol must stay disabled, keep her ban marker, and also be org-tagged, got %+v", st)
	}
	if !dir.hasCall("DisableUser:carol|acme") {
		t.Errorf("org disable must also record its suspension on the already-banned carol")
	}
	// All acme agents revoked; globex agent untouched.
	for _, id := range []string{"ar", "ac"} {
		if st := agentStatus(t, s, id); st != "revoked" {
			t.Errorf("acme agent %s status = %q, want revoked", id, st)
		}
	}
	if st := agentStatus(t, s, "gx"); st != "active" {
		t.Errorf("globex agent must stay active, got %q", st)
	}
}

func TestOrgEnable_ReenablesOnlyOrgTagged_LeavesAgentsSuspended(t *testing.T) {
	mux, s, dir, _ := seedOrgLifecycle(t)
	doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil)

	rec := doReq(t, mux, "POST", "/v1/orgs/acme/enable", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body.String())
	}
	if org, _, _ := s.GetOrg(t.Context(), "acme"); org.Disabled {
		t.Errorf("org acme Disabled flag should be cleared")
	}
	// Members the org disabled are back, tags cleared.
	for _, u := range []string{"alice", "bob"} {
		st := dir.users[u]
		if st == nil || !st.enabled || len(st.disabledByOrg) != 0 {
			t.Errorf("org-disabled member %s should be re-enabled + untagged, got %+v", u, st)
		}
	}
	// The admin-banned member stays disabled: org enable removes acme from her set,
	// but the separate ban marker keeps her locked out (a ban can't be laundered away
	// by an org-enable).
	if st := dir.users["carol"]; st == nil || st.enabled || !st.disabledByAdmin {
		t.Errorf("admin-banned carol must STAY disabled after org enable, got %+v", st)
	}
	// Agents stay suspended (asymmetric — manual resume).
	for _, id := range []string{"ar", "ac"} {
		if st := agentStatus(t, s, id); st != "revoked" {
			t.Errorf("agent %s must STAY revoked after org enable, got %q", id, st)
		}
	}
}

// TestOrgDisable_MultiOrgMemberStaysDisabledUntilAllEnable proves the multi-valued
// set semantics end-to-end at the registry: a member of TWO orgs disabled by both
// stays locked out until BOTH lift their suspension.
func TestOrgDisable_MultiOrgMemberStaysDisabledUntilAllEnable(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)

	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "acme", "adminUser": "alice"}); rec.Code != http.StatusCreated {
		t.Fatalf("create acme: %d", rec.Code)
	}
	if rec := doReq(t, mux, "POST", "/v1/orgs", map[string]string{"orgId": "globex", "adminUser": "dave"}); rec.Code != http.StatusCreated {
		t.Fatalf("create globex: %d", rec.Code)
	}
	// eve belongs to BOTH orgs.
	for _, org := range []string{"acme", "globex"} {
		if rec := doReq(t, mux, "POST", "/v1/orgs/"+org+"/members", map[string]string{"user": "eve"}); rec.Code != http.StatusCreated {
			t.Fatalf("add eve to %s: %d", org, rec.Code)
		}
	}

	// Both orgs suspend → eve is disabled by BOTH (the set carries both).
	doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil)
	doReq(t, mux, "POST", "/v1/orgs/globex/disable", nil)
	if st := dir.users["eve"]; st == nil || st.enabled || !st.hasOrg("acme") || !st.hasOrg("globex") {
		t.Fatalf("eve should be disabled by BOTH orgs, got %+v", st)
	}

	// acme lifts — globex still suspends eve, so she STAYS disabled.
	doReq(t, mux, "POST", "/v1/orgs/acme/enable", nil)
	if st := dir.users["eve"]; st == nil || st.enabled || st.hasOrg("acme") || !st.hasOrg("globex") {
		t.Fatalf("eve must stay disabled while globex still suspends her, got %+v", st)
	}

	// globex lifts — the set is now empty, so eve is finally re-enabled.
	doReq(t, mux, "POST", "/v1/orgs/globex/enable", nil)
	if st := dir.users["eve"]; st == nil || !st.enabled || len(st.disabledByOrg) != 0 {
		t.Fatalf("eve should be enabled once BOTH orgs lifted, got %+v", st)
	}
}

func TestOrgDisableEnable_Idempotent(t *testing.T) {
	mux, s, dir, _ := seedOrgLifecycle(t)

	for i := 0; i < 2; i++ {
		if rec := doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil); rec.Code != http.StatusOK {
			t.Fatalf("disable #%d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	// carol accrues acme EXACTLY ONCE across repeat disables (dedup), and keeps her
	// separate admin ban intact.
	if st := dir.users["carol"]; st == nil || !st.disabledByAdmin || len(st.disabledByOrg) != 1 || !st.hasOrg("acme") {
		t.Errorf("carol must accrue acme once (dedup) and keep her ban, got %+v", st)
	}
	for i := 0; i < 2; i++ {
		if rec := doReq(t, mux, "POST", "/v1/orgs/acme/enable", nil); rec.Code != http.StatusOK {
			t.Fatalf("enable #%d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if org, _, _ := s.GetOrg(t.Context(), "acme"); org.Disabled {
		t.Errorf("org should end enabled after repeat enable")
	}
	if st := dir.users["alice"]; st == nil || !st.enabled {
		t.Errorf("alice should be enabled after repeat enable, got %+v", st)
	}
}

func TestOrgDelete_OnDisabled_ClearsOrgTags(t *testing.T) {
	mux, _, dir, _ := seedOrgLifecycle(t)
	doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil)
	// Precondition: alice was org-tagged + disabled.
	if st := dir.users["alice"]; st == nil || st.enabled || !st.hasOrg("acme") {
		t.Fatalf("precondition: alice should be org-disabled, got %+v", st)
	}

	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	// Deleting a disabled org must not strand its org-locked members.
	if st := dir.users["alice"]; st == nil || !st.enabled || len(st.disabledByOrg) != 0 {
		t.Errorf("alice should be re-enabled + untagged after deleting the disabled org, got %+v", st)
	}
	// The admin-banned member stays disabled (delete hygiene only lifts THIS org's
	// suspension; the separate admin ban survives).
	if st := dir.users["carol"]; st == nil || st.enabled || !st.disabledByAdmin {
		t.Errorf("admin-banned carol must stay disabled after org delete, got %+v", st)
	}
}

// registerWorkerInto drives the full spawn path (preregister into orgID, then SVID
// self-register) and returns the two status codes so a test can assert where a
// disabled-org spawn guard bites. Empty orgID skips the preregister.
func spawnGuardMux(t *testing.T) (http.Handler, *store) {
	t.Helper()
	s := newStore()
	s.putTemplate(workerTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/agent-guard"}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll(), testDir())
	return mux, s
}

func preregister(t *testing.T, mux http.Handler, agentID, orgID string) int {
	t.Helper()
	body, _ := json.Marshal(registry.AgentRecord{AgentID: agentID, AgentType: "worker", UserID: "bob", OrgID: orgID})
	req := httptest.NewRequest("POST", "/v1/agents/preregister", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func selfRegister(t *testing.T, mux http.Handler) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"agentType": "worker", "userId": "bob"})
	req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-svid")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec.Code
}

func TestResumeGuard_RefusedWhileOrgDisabled(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	mux := orgMux(s, sdb, dir)
	ctx := t.Context()
	enc := spicedb.EncodeObjectID
	// A revoked org agent owned by bob, with the owner+org tuples production writes.
	s.PutOrg(ctx, registry.Organization{OrgID: "acme"})
	s.registerAgent(registry.AgentRecord{AgentID: "ar", AgentType: "worker", UserID: "bob", OrgID: "acme", Status: "revoked"})
	sdb.WriteRelationship(ctx, "agent:ar", "owner", "user:"+enc("bob"))
	sdb.WriteRelationship(ctx, "agent:ar", "org", "organization:"+enc("acme"))

	// While the org is disabled, the owner may NOT resume (undo the suspension).
	org, _, _ := s.GetOrg(ctx, "acme")
	org.Disabled = true
	s.PutOrg(ctx, org)
	if rec := doReq(t, mux, "POST", "/v1/agents/ar/resume?userId=bob", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("resume while org disabled: got %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if st := agentStatus(t, s, "ar"); st != "revoked" {
		t.Errorf("agent must stay revoked when resume is refused, got %q", st)
	}

	// Once the org is enabled, the manual resume works.
	org.Disabled = false
	s.PutOrg(ctx, org)
	if rec := doReq(t, mux, "POST", "/v1/agents/ar/resume?userId=bob", nil); rec.Code != http.StatusOK {
		t.Fatalf("resume after org enable: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st := agentStatus(t, s, "ar"); st != "active" {
		t.Errorf("agent should be active after resume, got %q", st)
	}
}

func TestAddMember_RefusedOnDisabledOrg(t *testing.T) {
	mux, _, _, _ := seedOrgLifecycle(t)
	doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil)
	if rec := doReq(t, mux, "POST", "/v1/orgs/acme/members", map[string]string{"user": "newguy"}); rec.Code != http.StatusConflict {
		t.Fatalf("add member to disabled org: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemoveMember_WhileOrgDisabled_ClearsOrgTagOnlyForTagged(t *testing.T) {
	mux, _, dir, _ := seedOrgLifecycle(t)
	doReq(t, mux, "POST", "/v1/orgs/acme/disable", nil)
	// bob was org-disabled (tagged acme). Removing him must clear the lock so he
	// isn't stranded off the member list.
	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/members/bob", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove bob: %d %s", rec.Code, rec.Body.String())
	}
	if st := dir.users["bob"]; st == nil || !st.enabled || len(st.disabledByOrg) != 0 {
		t.Errorf("org-tagged bob should be re-enabled + untagged on removal, got %+v", st)
	}
	// carol is admin-banned. Removing her lifts THIS org's suspension but the ban
	// keeps her disabled — removal must NOT launder the ban away.
	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/members/carol", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove carol: %d %s", rec.Code, rec.Body.String())
	}
	if st := dir.users["carol"]; st == nil || st.enabled || !st.disabledByAdmin {
		t.Errorf("admin-banned carol must stay disabled on removal, got %+v", st)
	}
}

func TestSpawnGuard_PreregisterRejectedForDisabledOrg(t *testing.T) {
	mux, s := spawnGuardMux(t)
	s.PutOrg(t.Context(), registry.Organization{OrgID: "acme", Disabled: true})

	if code := preregister(t, mux, "agent-guard", "acme"); code != http.StatusForbidden {
		t.Errorf("preregister into disabled org: got %d, want 403", code)
	}
}

func TestSpawnGuard_PreregisterAllowedForEnabledOrgAndOrgless(t *testing.T) {
	mux, s := spawnGuardMux(t)
	s.PutOrg(t.Context(), registry.Organization{OrgID: "acme"}) // enabled

	if code := preregister(t, mux, "agent-guard", "acme"); code != http.StatusCreated {
		t.Errorf("preregister into enabled org: got %d, want 201", code)
	}
	if code := preregister(t, mux, "agent-guard2", ""); code != http.StatusCreated {
		t.Errorf("preregister org-less: got %d, want 201", code)
	}
}

// TestSpawnGuard_SelfRegisterRejectedForDisabledOrg covers a token minted before
// the org was disabled (or a remote-agent anchor): the record was preregistered
// into the org while it was enabled, the org is then disabled, and the sidecar's
// SVID self-registration must be refused (defense in depth on POST /v1/agents).
func TestSpawnGuard_SelfRegisterRejectedForDisabledOrg(t *testing.T) {
	mux, s := spawnGuardMux(t)
	s.PutOrg(t.Context(), registry.Organization{OrgID: "acme"}) // enabled at preregister
	if code := preregister(t, mux, "agent-guard", "acme"); code != http.StatusCreated {
		t.Fatalf("preregister: got %d, want 201", code)
	}
	// Org disabled after the token was minted.
	org, _, _ := s.GetOrg(t.Context(), "acme")
	org.Disabled = true
	s.PutOrg(t.Context(), org)

	if code := selfRegister(t, mux); code != http.StatusForbidden {
		t.Errorf("self-register into now-disabled org: got %d, want 403", code)
	}
}
