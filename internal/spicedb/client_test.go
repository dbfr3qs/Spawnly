package spicedb_test

import (
	"context"
	"testing"

	"github.com/agent-platform/poc/internal/spicedb"
)

func TestMockWriteCheckDelete(t *testing.T) {
	m := spicedb.NewMock()
	ctx := context.Background()

	if err := m.WriteRelationship(ctx, "tenant:t1", "agent", "agent:a1"); err != nil {
		t.Fatalf("write: %v", err)
	}

	ok, err := m.CheckPermission(ctx, "tenant:t1", "work_on", "agent:a1")
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !ok {
		t.Fatal("expected permission granted")
	}

	if err := m.DeleteAgentRelationships(ctx, "a1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	ok, _ = m.CheckPermission(ctx, "tenant:t1", "work_on", "agent:a1")
	if ok {
		t.Fatal("expected permission revoked after delete")
	}
}
