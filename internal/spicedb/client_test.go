package spicedb_test

import (
	"context"
	"testing"

	"github.com/spawnly/platform/internal/spicedb"
)

func TestMockWriteCheckDelete(t *testing.T) {
	m := spicedb.NewMock()
	ctx := context.Background()

	if err := m.WriteRelationship(ctx, "tenant:t1", "agent", "agent:a1"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// work_on = agent & agent->enabled, so the agent's enabled tuple is required.
	if err := m.WriteRelationship(ctx, "agent:a1", "enabled", "agent:a1"); err != nil {
		t.Fatalf("write enabled: %v", err)
	}

	ok, err := m.CheckPermission(ctx, "tenant:t1", "work_on", "agent:a1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !ok {
		t.Fatal("expected permission granted")
	}

	if err := m.DeleteAgentRelationships(ctx, "a1", []string{"tenant"}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ok, _ = m.CheckPermission(ctx, "tenant:t1", "work_on", "agent:a1")
	if ok {
		t.Fatal("expected permission revoked after delete")
	}
}

// TestMockAgentViewManage pins the tenant-visibility Phase 2 agent authority
// model in the mock: view == manage == owner + org->admin_agents. The owner and
// the org admin hold BOTH view and manage; a plain org MEMBER holds NEITHER (a
// member sees only agents they own), and a cross-org user holds neither.
func TestMockAgentViewManage(t *testing.T) {
	m := spicedb.NewMock()
	ctx := context.Background()
	enc := spicedb.EncodeObjectID

	// agent:a1 is owned by alice and scoped to org acme; carol is acme's admin,
	// dave is a plain acme member, eve is a member of a different org.
	m.WriteRelationship(ctx, "agent:a1", "owner", "user:"+enc("alice"))
	m.WriteRelationship(ctx, "agent:a1", "org", "organization:"+enc("acme"))
	m.WriteRelationship(ctx, "organization:"+enc("acme"), "admin", "user:"+enc("carol"))
	m.WriteRelationship(ctx, "organization:"+enc("acme"), "member", "user:"+enc("dave"))
	m.WriteRelationship(ctx, "organization:"+enc("globex"), "member", "user:"+enc("eve"))

	cases := []struct {
		user              string
		wantView, wantMng bool
	}{
		{"alice", true, true}, // owner
		{"carol", true, true}, // org admin
		{"dave", false, false}, // plain member: NO agent view (Phase 2)
		{"eve", false, false},  // cross-org
	}
	for _, c := range cases {
		subj := "user:" + enc(c.user)
		if got, _ := m.CheckPermission(ctx, "agent:a1", "view", subj); got != c.wantView {
			t.Errorf("%s view = %v, want %v", c.user, got, c.wantView)
		}
		if got, _ := m.CheckPermission(ctx, "agent:a1", "manage", subj); got != c.wantMng {
			t.Errorf("%s manage = %v, want %v", c.user, got, c.wantMng)
		}
	}

	// The org-membership permissions are UNCHANGED: dave is still a member
	// (view_agents) though he is not an admin (admin_agents). This is what
	// orgMemberCheck / /v1/my/orgs still rely on.
	if ok, _ := m.CheckPermission(ctx, "organization:"+enc("acme"), "view_agents", "user:"+enc("dave")); !ok {
		t.Error("dave should still satisfy org view_agents (membership) — only AGENT view tightened")
	}
	if ok, _ := m.CheckPermission(ctx, "organization:"+enc("acme"), "admin_agents", "user:"+enc("dave")); ok {
		t.Error("dave is not an org admin")
	}
}

// TestMockDeleteScopedToResourceType proves the delete filter is scoped to the
// resource types passed in, not just the agent subject — a tuple on a resource
// type outside the list must survive. This guards against the old hardcoded
// "tenant" behavior and ensures a caller that omits a type leaks it (so the
// caller's relationResourceTypes derivation is the thing under test elsewhere).
func TestMockDeleteScopedToResourceType(t *testing.T) {
	m := spicedb.NewMock()
	ctx := context.Background()

	if err := m.WriteRelationship(ctx, "tenant:t1", "agent", "agent:a1"); err != nil {
		t.Fatalf("write tenant: %v", err)
	}
	if err := m.WriteRelationship(ctx, "project:p1", "agent", "agent:a1"); err != nil {
		t.Fatalf("write project: %v", err)
	}
	if err := m.WriteRelationship(ctx, "agent:a1", "enabled", "agent:a1"); err != nil {
		t.Fatalf("write enabled: %v", err)
	}

	// Delete only the "tenant"-typed relationship.
	if err := m.DeleteAgentRelationships(ctx, "a1", []string{"tenant"}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if ok, _ := m.CheckPermission(ctx, "tenant:t1", "work_on", "agent:a1"); ok {
		t.Fatal("tenant tuple should have been deleted")
	}
	if ok, _ := m.CheckPermission(ctx, "project:p1", "work_on", "agent:a1"); !ok {
		t.Fatal("project tuple should have survived a tenant-only delete")
	}

	// An empty resource-type set is a no-op.
	if err := m.DeleteAgentRelationships(ctx, "a1", nil); err != nil {
		t.Fatalf("delete nil: %v", err)
	}
	if ok, _ := m.CheckPermission(ctx, "project:p1", "work_on", "agent:a1"); !ok {
		t.Fatal("project tuple should survive a no-op delete")
	}
}
