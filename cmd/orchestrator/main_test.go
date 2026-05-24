// cmd/orchestrator/main_test.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/spicedb"
)

func TestSpawnCreatesAgentWorkload(t *testing.T) {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)

	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb)

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
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)

	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb)

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
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)

	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, sdb)

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
