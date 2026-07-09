package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// --- Org-scoped template create/list/manage (Phase 6) -------------------------

func makeTemplateBody(agentType, bodyOrg string) registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: agentType, Version: "1", Status: "active", OrgID: bodyOrg,
		Runtime: registry.RuntimeSpec{Lifecycle: "short-lived"},
		AuthZ:   registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{}},
	}
}

func getTemplateOrg(t *testing.T, mux http.Handler, agentType string) (registry.AgentTemplate, int) {
	t.Helper()
	rec := doReq(t, mux, "GET", "/v1/templates/"+agentType, nil)
	if rec.Code != http.StatusOK {
		return registry.AgentTemplate{}, rec.Code
	}
	var tpl registry.AgentTemplate
	json.Unmarshal(rec.Body.Bytes(), &tpl)
	return tpl, rec.Code
}

func TestTemplateCreate_OrgStampFromQueryNotBody(t *testing.T) {
	mux := orgMux(newStore(), spicedb.NewMock(), newFakeDirectory())
	// Body claims globex; the trusted ?orgId=acme must win, body is ignored.
	rec := doReq(t, mux, "POST", "/v1/templates?orgId=acme", makeTemplateBody("scoped", "globex"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d (%s)", rec.Code, rec.Body.String())
	}
	tpl, code := getTemplateOrg(t, mux, "scoped")
	if code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	if tpl.OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme (from query, not body's globex)", tpl.OrgID)
	}
}

func TestTemplateCreate_NoOrgIsGlobal(t *testing.T) {
	mux := orgMux(newStore(), spicedb.NewMock(), newFakeDirectory())
	rec := doReq(t, mux, "POST", "/v1/templates", makeTemplateBody("glob", "sneaky"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d", rec.Code)
	}
	tpl, _ := getTemplateOrg(t, mux, "glob")
	if tpl.OrgID != "" {
		t.Errorf("OrgID = %q, want empty (global) — body value must be ignored", tpl.OrgID)
	}
}

func TestTemplateCreate_NoCrossScopeHijack(t *testing.T) {
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	// A global template + a globex template already exist.
	doReq(t, mux, "POST", "/v1/templates", makeTemplateBody("shared", ""))
	doReq(t, mux, "POST", "/v1/templates?orgId=globex", makeTemplateBody("gx", ""))

	// An acme org admin CANNOT steal the global name into acme (409, scope unchanged).
	if rec := doReq(t, mux, "POST", "/v1/templates?orgId=acme", makeTemplateBody("shared", "")); rec.Code != http.StatusConflict {
		t.Errorf("hijack global: %d, want 409", rec.Code)
	}
	if tpl, _ := getTemplateOrg(t, mux, "shared"); tpl.OrgID != "" {
		t.Errorf("global template scope was mutated to %q", tpl.OrgID)
	}
	// Nor another org's template.
	if rec := doReq(t, mux, "POST", "/v1/templates?orgId=acme", makeTemplateBody("gx", "")); rec.Code != http.StatusConflict {
		t.Errorf("hijack cross-org: %d, want 409", rec.Code)
	}
	if tpl, _ := getTemplateOrg(t, mux, "gx"); tpl.OrgID != "globex" {
		t.Errorf("globex template scope was mutated to %q", tpl.OrgID)
	}
	// The owning org admin CAN re-create (edit) their own template of that name.
	if rec := doReq(t, mux, "POST", "/v1/templates?orgId=globex", makeTemplateBody("gx", "")); rec.Code != http.StatusCreated {
		t.Errorf("own-org edit: %d, want 201", rec.Code)
	}
	// A realm admin (no orgId) editing an org template PRESERVES its scope (no de-scope).
	if rec := doReq(t, mux, "POST", "/v1/templates", makeTemplateBody("gx", "")); rec.Code != http.StatusCreated {
		t.Errorf("realm admin edit org template: %d, want 201", rec.Code)
	}
	if tpl, _ := getTemplateOrg(t, mux, "gx"); tpl.OrgID != "globex" {
		t.Errorf("realm admin edit de-scoped gx to %q, want globex preserved", tpl.OrgID)
	}
}

func TestTemplateList_OrgVisibility(t *testing.T) {
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	doReq(t, mux, "POST", "/v1/templates", makeTemplateBody("global-1", ""))
	doReq(t, mux, "POST", "/v1/templates?orgId=acme", makeTemplateBody("acme-1", ""))
	doReq(t, mux, "POST", "/v1/templates?orgId=globex", makeTemplateBody("globex-1", ""))

	names := func(path string) map[string]bool {
		rec := doReq(t, mux, "GET", path, nil)
		var out []string
		json.Unmarshal(rec.Body.Bytes(), &out)
		m := map[string]bool{}
		for _, n := range out {
			m[n] = true
		}
		return m
	}

	// Bare list + detail=spawn: acme sees global + acme, NOT globex.
	for _, path := range []string{"/v1/templates?orgId=acme", "/v1/templates?detail=spawn&orgId=acme"} {
		var got map[string]bool
		if path == "/v1/templates?orgId=acme" {
			got = names(path)
		} else {
			rec := doReq(t, mux, "GET", path, nil)
			var arr []map[string]any
			json.Unmarshal(rec.Body.Bytes(), &arr)
			got = map[string]bool{}
			for _, e := range arr {
				got[e["agentType"].(string)] = true
			}
		}
		if !got["global-1"] || !got["acme-1"] || got["globex-1"] {
			t.Errorf("%s visibility = %v, want global-1+acme-1, not globex-1", path, got)
		}
	}

	// Org-less caller (no orgId): global only.
	got := names("/v1/templates")
	if !got["global-1"] || got["acme-1"] || got["globex-1"] {
		t.Errorf("org-less bare list = %v, want global-1 only", got)
	}

	// detail=full with orgId=acme: acme's OWN templates only (excludes global).
	rec := doReq(t, mux, "GET", "/v1/templates?detail=full&orgId=acme", nil)
	var full []registry.AgentTemplate
	json.Unmarshal(rec.Body.Bytes(), &full)
	seen := map[string]bool{}
	for _, tpl := range full {
		seen[tpl.AgentType] = true
	}
	if !seen["acme-1"] || seen["global-1"] || seen["globex-1"] {
		t.Errorf("detail=full orgId=acme = %v, want acme-1 only", seen)
	}
}

func TestTemplateManage_OrgScopeEnforced(t *testing.T) {
	s := newStore()
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	doReq(t, mux, "POST", "/v1/templates?orgId=acme", makeTemplateBody("acme-t", ""))
	doReq(t, mux, "POST", "/v1/templates", makeTemplateBody("global-t", ""))

	// An org admin scoped to globex cannot disable acme's template → 404 (no oracle).
	if rec := doReq(t, mux, "PATCH", "/v1/templates/acme-t?orgId=globex", map[string]string{"status": "disabled"}); rec.Code != http.StatusNotFound {
		t.Errorf("cross-org PATCH: %d, want 404", rec.Code)
	}
	// Nor the global catalog.
	if rec := doReq(t, mux, "PATCH", "/v1/templates/global-t?orgId=acme", map[string]string{"status": "disabled"}); rec.Code != http.StatusNotFound {
		t.Errorf("org admin touching global PATCH: %d, want 404", rec.Code)
	}
	// Its OWN org's template: allowed.
	if rec := doReq(t, mux, "PATCH", "/v1/templates/acme-t?orgId=acme", map[string]string{"status": "disabled"}); rec.Code != http.StatusOK {
		t.Errorf("own-org PATCH: %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	// DELETE cross-org → 404 even though now disabled.
	if rec := doReq(t, mux, "DELETE", "/v1/templates/acme-t?orgId=globex", nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-org DELETE: %d, want 404", rec.Code)
	}
	// DELETE own org (disabled) → 204.
	if rec := doReq(t, mux, "DELETE", "/v1/templates/acme-t?orgId=acme", nil); rec.Code != http.StatusNoContent {
		t.Errorf("own-org DELETE: %d, want 204", rec.Code)
	}
	// Realm admin (no orgId) can disable + delete the global template.
	if rec := doReq(t, mux, "PATCH", "/v1/templates/global-t", map[string]string{"status": "disabled"}); rec.Code != http.StatusOK {
		t.Errorf("realm admin PATCH global: %d", rec.Code)
	}
	if rec := doReq(t, mux, "DELETE", "/v1/templates/global-t", nil); rec.Code != http.StatusNoContent {
		t.Errorf("realm admin DELETE global: %d", rec.Code)
	}
}

// --- Removed-member disposition (Phase 6) -------------------------------------

// seedMemberAgents seeds an org with a removal disposition, a member, and a
// two-node agent tree (root + child) owned by that member in the org, with the
// owner + enabled + org tuples written as production would.
func seedMemberAgents(t *testing.T, s registry.Store, sdb *spicedb.Mock, dir *fakeDirectory, org, disposition, user string) {
	t.Helper()
	ctx := context.Background()
	if err := s.PutOrg(ctx, registry.Organization{OrgID: org, RemovalDisposition: disposition}); err != nil {
		t.Fatal(err)
	}
	dir.orgs[org] = true
	if dir.members[org] == nil {
		dir.members[org] = map[string]bool{}
	}
	dir.members[org][user] = true
	enc := spicedb.EncodeObjectID
	sub := "user:" + enc(user)
	sdb.WriteRelationship(ctx, "organization:"+enc(org), "member", sub)
	for _, a := range []registry.AgentRecord{
		{AgentID: "root", AgentType: "worker", UserID: user, OrgID: org, Status: "active"},
		{AgentID: "child", AgentType: "worker", UserID: user, OrgID: org, Status: "active", ParentID: "root"},
	} {
		if err := s.RegisterAgent(ctx, a); err != nil {
			t.Fatal(err)
		}
		sdb.WriteRelationship(ctx, "agent:"+a.AgentID, "owner", sub)
		sdb.WriteRelationship(ctx, "agent:"+a.AgentID, "org", "organization:"+enc(org))
		sdb.WriteRelationship(ctx, "agent:"+a.AgentID, "enabled", "agent:"+a.AgentID)
	}
}

func TestRemoveMember_AutoRevoke(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	seedMemberAgents(t, s, sdb, dir, "acme", registry.RemovalAutoRevoke, "viewer")
	mux := orgMux(s, sdb, dir)

	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/members/viewer", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove member: %d (%s)", rec.Code, rec.Body.String())
	}
	// Both nodes flipped to revoked; enabled tuples dropped.
	for _, id := range []string{"root", "child"} {
		a, _ := s.GetAgent(context.Background(), id)
		if a.Status != "revoked" {
			t.Errorf("agent %s status = %q, want revoked", id, a.Status)
		}
		if sdb.HasRelationship("agent:"+id, "enabled", "agent:"+id) {
			t.Errorf("agent %s: enabled tuple should be gone after auto-revoke", id)
		}
		// The ex-member's owner tuple is also dropped, so they lose lifecycle access.
		if sdb.HasRelationship("agent:"+id, "owner", "user:"+spicedb.EncodeObjectID("viewer")) {
			t.Errorf("agent %s: owner tuple should be dropped on removal (ex-member loses access)", id)
		}
	}
	// Membership tuple gone → ex-member loses access regardless.
	if sdb.HasRelationship("organization:"+spicedb.EncodeObjectID("acme"), "member", "user:"+spicedb.EncodeObjectID("viewer")) {
		t.Errorf("membership tuple should be dropped")
	}
}

func TestRemoveMember_OrphanToAdmin(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	seedMemberAgents(t, s, sdb, dir, "acme", registry.RemovalOrphanToAdmin, "viewer")
	// An org admin exists, so org->admin_agents keeps manage after orphaning.
	sdb.WriteRelationship(context.Background(), "organization:"+spicedb.EncodeObjectID("acme"), "admin", "user:"+spicedb.EncodeObjectID("alice"))
	mux := orgMux(s, sdb, dir)

	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/members/viewer", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("remove member: %d (%s)", rec.Code, rec.Body.String())
	}
	enc := spicedb.EncodeObjectID
	for _, id := range []string{"root", "child"} {
		a, _ := s.GetAgent(context.Background(), id)
		if a.Status != "active" {
			t.Errorf("agent %s status = %q, want active (orphan keeps it running)", id, a.Status)
		}
		if a.UserID != "viewer" {
			t.Errorf("agent %s UserID = %q, want viewer preserved for history", id, a.UserID)
		}
		// Owner tuple dropped, but org tuple (→ admin manage) remains.
		if sdb.HasRelationship("agent:"+id, "owner", "user:"+enc("viewer")) {
			t.Errorf("agent %s: owner tuple should be dropped on orphan", id)
		}
		// Org admin still manages via org->admin_agents.
		if ok, _ := sdb.CheckPermission(context.Background(), "agent:"+id, "manage", "user:"+enc("alice")); !ok {
			t.Errorf("agent %s: org admin should still hold manage after orphan", id)
		}
	}
}

// TestRemoveMember_DispositionScopedToOwner: an agent owned by ANOTHER member in
// the same org is untouched by a removal disposition.
func TestRemoveMember_DispositionScopedToOwner(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	dir := newFakeDirectory()
	seedMemberAgents(t, s, sdb, dir, "acme", registry.RemovalAutoRevoke, "viewer")
	ctx := context.Background()
	// Another member's active agent in the same org.
	s.RegisterAgent(ctx, registry.AgentRecord{AgentID: "other", AgentType: "worker", UserID: "someone", OrgID: "acme", Status: "active"})
	mux := orgMux(s, sdb, dir)

	doReq(t, mux, "DELETE", "/v1/orgs/acme/members/viewer", nil)
	if a, _ := s.GetAgent(ctx, "other"); a.Status != "active" {
		t.Errorf("other member's agent status = %q, want active (not revoked)", a.Status)
	}
}
