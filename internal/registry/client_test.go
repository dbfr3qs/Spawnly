// internal/registry/client_test.go
package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-platform/poc/internal/registry"
)

func TestHTTPClientGetTemplate(t *testing.T) {
	tpl := registry.AgentTemplate{
		AgentType: "worker",
		Runtime:   registry.RuntimeSpec{Image: "agent-agent:latest"},
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
	if got.Runtime.Image != "agent-agent:latest" {
		t.Errorf("got image %q, want %q", got.Runtime.Image, "agent-agent:latest")
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
