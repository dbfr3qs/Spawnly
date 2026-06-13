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
