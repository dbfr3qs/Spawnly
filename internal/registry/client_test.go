// internal/registry/client_test.go
package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/registry"
)

func TestHTTPClientGetTemplate(t *testing.T) {
	tpl := registry.AgentTemplate{
		AgentType: "worker",
		Runtime:   registry.RuntimeSpec{Image: "agent-go-worker:latest"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/templates/worker" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tpl)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := registry.New(srv.URL)
	got, err := client.GetTemplate(context.Background(), "worker")
	if err != nil {
		t.Fatalf("GetTemplate: %v", err)
	}
	if got.Runtime.Image != "agent-go-worker:latest" {
		t.Errorf("got image %q, want %q", got.Runtime.Image, "agent-go-worker:latest")
	}
}

func TestHTTPClientGetTemplateNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := registry.New(srv.URL)
	_, err := client.GetTemplate(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for not found template")
	}
}

func TestHTTPClientCheckSpawnPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/spawn-policy" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("childType") == "child-agent" {
			json.NewEncoder(w).Encode(registry.SpawnDecision{Allowed: true})
		} else {
			json.NewEncoder(w).Encode(registry.SpawnDecision{Allowed: false, Reason: "denied"})
		}
	}))
	defer srv.Close()

	client := registry.New(srv.URL)
	if d, err := client.CheckSpawnPolicy(context.Background(), "parent-1", "child-agent"); err != nil || !d.Allowed {
		t.Fatalf("allowed case: err=%v, decision=%+v", err, d)
	}
	if d, err := client.CheckSpawnPolicy(context.Background(), "parent-1", "other"); err != nil || d.Allowed {
		t.Fatalf("denied case: err=%v, decision=%+v", err, d)
	}
}

func TestMockClientSpawnPolicy(t *testing.T) {
	m := registry.NewMock(map[string]registry.AgentTemplate{
		"parent-agent": {
			AgentType:  "parent-agent",
			Delegation: registry.DelegationPolicy{AllowedChildTypes: []string{"child-agent"}},
		},
	})
	m.PreRegisterAgent(context.Background(), registry.AgentRecord{AgentID: "parent-1", AgentType: "parent-agent"})

	if d, _ := m.CheckSpawnPolicy(context.Background(), "parent-1", "child-agent"); !d.Allowed {
		t.Fatalf("expected allowed for listed child, got %+v", d)
	}
	if d, _ := m.CheckSpawnPolicy(context.Background(), "parent-1", "other"); d.Allowed {
		t.Fatalf("expected denied for unlisted child, got %+v", d)
	}
	if d, _ := m.CheckSpawnPolicy(context.Background(), "ghost", "child-agent"); d.Allowed {
		t.Fatalf("expected denied for unknown parent, got %+v", d)
	}
}

func TestMockClient(t *testing.T) {
	tpl := registry.AgentTemplate{AgentType: "worker"}
	m := registry.NewMock(map[string]registry.AgentTemplate{"worker": tpl})

	got, err := m.GetTemplate(context.Background(), "worker")
	if err != nil || got.AgentType != "worker" {
		t.Fatalf("GetTemplate: err=%v, got=%+v", err, got)
	}

	m.Complete(context.Background(), "agent-1")
	m.Fail(context.Background(), "agent-2")
	if len(m.Completed) != 1 || m.Completed[0] != "agent-1" {
		t.Errorf("unexpected Completed: %v", m.Completed)
	}
	if len(m.Failed) != 1 || m.Failed[0] != "agent-2" {
		t.Errorf("unexpected Failed: %v", m.Failed)
	}
}
