// cmd/orchestrator/main_test.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/spicedb"
)

// newTestMux creates a mux wired to a mock registry httptest.Server that handles
// the endpoints used by the event emission goroutine and the proxy handlers.
func newTestMux(t *testing.T, regHandler http.Handler) (*http.ServeMux, *fake.ClientBuilder) {
	t.Helper()
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)

	cb := fake.NewClientBuilder().WithScheme(s)
	return nil, cb // placeholder; callers build mux themselves
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)
	return s
}

// defaultMockRegistry returns an httptest.Server that responds to:
//   - POST /v1/agents/*/events  → 201
//   - GET  /v1/agents           → []
func defaultMockRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSpawnCreatesAgentWorkload(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb, mockReg.URL)

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "worker",
		UserID:    "user-1",
		TenantID:  "tenant-1",
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}

	var resp SpawnResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.WorkloadName == "" {
		t.Fatal("expected workloadName in response")
	}

	// Verify AgentWorkload created in fake client
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 AgentWorkload, got %d", len(list.Items))
	}
	if list.Items[0].Spec.TenantID != "tenant-1" {
		t.Fatalf("unexpected tenantId: %q", list.Items[0].Spec.TenantID)
	}
	if list.Items[0].Spec.UserID != "user-1" {
		t.Fatalf("unexpected userId: %q", list.Items[0].Spec.UserID)
	}
	if list.Items[0].Spec.AgentType != "worker" {
		t.Fatalf("unexpected agentType: %q", list.Items[0].Spec.AgentType)
	}
	if list.Items[0].Spec.Lifecycle != "short-lived" {
		t.Fatalf("unexpected lifecycle: %q", list.Items[0].Spec.Lifecycle)
	}
	if list.Items[0].Name != resp.WorkloadName {
		t.Fatalf("CRD name %q does not match response workloadName %q", list.Items[0].Name, resp.WorkloadName)
	}
}

func TestSpawnMissingRequiredFields(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb, mockReg.URL)

	// Missing userId
	body, _ := json.Marshal(map[string]string{"agentType": "worker", "tenantId": "t1"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestSpawnDefaultAgentType(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb, mockReg.URL)

	body, _ := json.Marshal(map[string]string{"userId": "u1", "tenantId": "t1"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}

	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if list.Items[0].Spec.AgentType != "worker" {
		t.Fatalf("expected default agentType worker, got %q", list.Items[0].Spec.AgentType)
	}
}

func TestSpawnWithTask(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb, mockReg.URL)

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "worker",
		UserID:    "user-2",
		TenantID:  "tenant-2",
		Task:      "hello",
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202", rec.Code)
	}

	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 AgentWorkload, got %d", len(list.Items))
	}
	if list.Items[0].Spec.Task != "hello" {
		t.Fatalf("expected Spec.Task == \"hello\", got %q", list.Items[0].Spec.Task)
	}
}

func TestListAgentsProxiesToRegistry(t *testing.T) {
	mockReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"agentId":"a1"}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb, mockReg.URL)

	req := httptest.NewRequest("GET", "/v1/agents", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}

	var agents []map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&agents); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(agents) != 1 || agents[0]["agentId"] != "a1" {
		t.Fatalf("unexpected agents response: %v", agents)
	}
}

func TestDeleteAgent_NotFound(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb, mockReg.URL)

	req := httptest.NewRequest("DELETE", "/v1/agents/missing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}
