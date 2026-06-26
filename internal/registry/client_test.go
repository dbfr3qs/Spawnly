// internal/registry/client_test.go
package registry_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/registry"
)

// TestHTTPClientTimeout verifies the registry client gives up on a server that
// never finishes responding, rather than hanging forever. The default client
// timeout is 30s; we point it at a server that sleeps far longer and assert the
// call returns an error well before that would complete.
func TestHTTPClientTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until the test tears the server down; the client must time out.
		<-release
	}))
	defer srv.Close()
	defer close(release)

	client := registry.New(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := client.GetTemplate(ctx, "worker")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout/cancellation error from a non-responding server")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client hung on a non-responding server; expected it to give up")
	}
}

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

func TestHTTPClientGetAgent(t *testing.T) {
	rec := registry.AgentRecord{
		AgentID:  "parent-1",
		UserID:   "alice",
		TenantID: "t1",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents/parent-1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(rec)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := registry.New(srv.URL)

	got, err := client.GetAgent(context.Background(), "parent-1")
	if err != nil {
		t.Fatalf("GetAgent(parent-1): %v", err)
	}
	if got.UserID != "alice" || got.TenantID != "t1" {
		t.Errorf("got %+v, want userId=alice tenantId=t1", got)
	}

	if _, err := client.GetAgent(context.Background(), "ghost"); err == nil {
		t.Fatal("expected error for unknown agent")
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

func TestHTTPClientSubtree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/agents/root/subtree" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string][]string{"subtree": {"root", "a", "b"}})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	client := registry.New(srv.URL)

	got, err := client.Subtree(context.Background(), "root", "alice")
	if err != nil {
		t.Fatalf("Subtree(root): %v", err)
	}
	want := []string{"root", "a", "b"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}

	// Unknown id → (nil, nil), not an error.
	got, err = client.Subtree(context.Background(), "ghost", "alice")
	if err != nil {
		t.Fatalf("Subtree(ghost): unexpected error %v", err)
	}
	if got != nil {
		t.Fatalf("Subtree(ghost): expected nil, got %v", got)
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

// TestNewWithToken_SendsBearer verifies that NewWithToken attaches
// "Authorization: Bearer <token>" to outbound requests, and that New (token "")
// sends no Authorization header. Exercised via Complete (a PATCH).
func TestNewWithToken_SendsBearer(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// With a token → request carries the bearer.
	if err := registry.NewWithToken(srv.URL, "tok").Complete(context.Background(), "agent-1"); err != nil {
		t.Fatalf("Complete (with token): %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("Authorization: got %q, want %q", gotAuth, "Bearer tok")
	}

	// Plain New → no Authorization header.
	gotAuth = ""
	if err := registry.New(srv.URL).Complete(context.Background(), "agent-1"); err != nil {
		t.Fatalf("Complete (no token): %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header, got %q", gotAuth)
	}
}
