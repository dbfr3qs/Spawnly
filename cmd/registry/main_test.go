// cmd/registry/main_test.go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-platform/poc/internal/registry"
	"github.com/agent-platform/poc/internal/spicedb"
	"github.com/agent-platform/poc/internal/spiffe"
)

func workerTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "worker", Version: "1.0.0", Status: "active",
		Runtime: registry.RuntimeSpec{
			Image:     "agent-agent:latest",
			Resources: registry.ResourceLimits{CPULimit: "500m", MemoryLimit: "256Mi"},
		},
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	}
}

func TestTemplateCRUD(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, validator)

	body, _ := json.Marshal(workerTemplate())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/templates", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create template: got %d, want 201", rec.Code)
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/templates/worker", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("get template: got %d, want 200", rec2.Code)
	}
	var got registry.AgentTemplate
	json.NewDecoder(rec2.Body).Decode(&got)
	if got.Runtime.Image != "agent-agent:latest" {
		t.Fatalf("unexpected image: %q", got.Runtime.Image)
	}
}

func TestAgentSelfRegistration(t *testing.T) {
	s := newStore()
	s.putTemplate(workerTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/agent-test"}
	mux := buildMux(s, sdb, validator)

	body, _ := json.Marshal(map[string]string{
		"agentType": "worker",
		"tenantId":  "tenant-1",
		"userId":    "user-1",
	})
	req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-svid")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("self-register: got %d, want 201", rec.Code)
	}

	agent := s.getAgent("agent-test")
	if agent.Status != "active" {
		t.Fatalf("expected status active, got %q", agent.Status)
	}

	ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:agent-test")
	if !ok {
		t.Fatal("expected SpiceDB tuple written on registration")
	}
}

func TestAgentISBackchannelCheck(t *testing.T) {
	s := newStore()
	s.registerAgent(registry.AgentRecord{AgentID: "agent-test", Status: "active"})
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, validator)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/agent-test", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get agent: got %d, want 200", rec.Code)
	}
	var got registry.AgentRecord
	json.NewDecoder(rec.Body).Decode(&got)
	if got.Status != "active" {
		t.Fatalf("expected active, got %q", got.Status)
	}
}

func TestAgentCompletion_DeletesSpiceDB(t *testing.T) {
	s := newStore()
	s.registerAgent(registry.AgentRecord{AgentID: "agent-test", TenantID: "tenant-1", Status: "active"})
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(t.Context(), "tenant:tenant-1", "agent", "agent:agent-test")
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, validator)

	patchBody, _ := json.Marshal(map[string]string{"status": "completed"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v1/agents/agent-test", bytes.NewReader(patchBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d, want 200", rec.Code)
	}

	if s.getAgent("agent-test").Status != "completed" {
		t.Fatal("status not updated")
	}

	ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:agent-test")
	if ok {
		t.Fatal("expected SpiceDB tuple deleted on completion")
	}
}
