// cmd/registry/main_test.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/controlplane"
	"github.com/spawnly/platform/internal/events"
	"github.com/spawnly/platform/internal/registrant"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/spiffe"
)

func workerTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "worker", Version: "1.0.0", Status: "active",
		Runtime: registry.RuntimeSpec{
			Image:     "agent-go-worker:latest",
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
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

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
	if got.Runtime.Image != "agent-go-worker:latest" {
		t.Fatalf("unexpected image: %q", got.Runtime.Image)
	}
}

// seedWorker POSTs the worker template and fails the test if creation didn't 201.
func seedWorker(t *testing.T, mux http.Handler) {
	t.Helper()
	body, _ := json.Marshal(workerTemplate())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/templates", bytes.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed worker: got %d, want 201", rec.Code)
	}
}

func listTemplateTypes(t *testing.T, mux http.Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/templates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list templates: got %d, want 200", rec.Code)
	}
	var types []string
	json.NewDecoder(rec.Body).Decode(&types)
	return types
}

func TestPatchTemplateStatus(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
	seedWorker(t, mux)

	patch := func(agentType, status string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"status": status})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v1/templates/"+agentType, bytes.NewReader(body)))
		return rec
	}

	// 200 on an existing type, response carries the updated template.
	rec := patch("worker", registry.TemplateStatusDisabled)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch disabled: got %d, want 200", rec.Code)
	}
	var got registry.AgentTemplate
	json.NewDecoder(rec.Body).Decode(&got)
	if got.Status != registry.TemplateStatusDisabled {
		t.Fatalf("patched status: got %q, want disabled", got.Status)
	}

	// 400 on an invalid status value.
	if rec := patch("worker", "bogus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch invalid status: got %d, want 400", rec.Code)
	}

	// 404 on an unknown type.
	if rec := patch("nope", registry.TemplateStatusActive); rec.Code != http.StatusNotFound {
		t.Fatalf("patch unknown: got %d, want 404", rec.Code)
	}

	// 400 on a malformed {type} segment: empty (bare path) or slash-bearing.
	if rec := patch("", registry.TemplateStatusActive); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch empty type: got %d, want 400", rec.Code)
	}
	if rec := patch("a/b", registry.TemplateStatusActive); rec.Code != http.StatusBadRequest {
		t.Fatalf("patch slash type: got %d, want 400", rec.Code)
	}
}

// TestCreateTemplateInvalidStatus asserts POST rejects an unrecognized status
// value (empty and the tolerated "deprecated" stay allowed via TestTemplateCRUD).
func TestCreateTemplateInvalidStatus(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	tpl := workerTemplate()
	tpl.Status = "bogus"
	body, _ := json.Marshal(tpl)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/templates", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create invalid status: got %d, want 400", rec.Code)
	}
}

func TestDisabledTemplateHiddenFromList(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
	seedWorker(t, mux)

	// Visible while active.
	if !slices.Contains(listTemplateTypes(t, mux), "worker") {
		t.Fatalf("worker should be listed while active")
	}

	// Disable it.
	body, _ := json.Marshal(map[string]string{"status": registry.TemplateStatusDisabled})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v1/templates/worker", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable worker: got %d, want 200", rec.Code)
	}

	// Gone from the list filter.
	if slices.Contains(listTemplateTypes(t, mux), "worker") {
		t.Fatalf("disabled worker should not be listed")
	}

	// But still fetchable by type, with Status==disabled.
	recGet := httptest.NewRecorder()
	mux.ServeHTTP(recGet, httptest.NewRequest("GET", "/v1/templates/worker", nil))
	if recGet.Code != http.StatusOK {
		t.Fatalf("get disabled worker: got %d, want 200", recGet.Code)
	}
	var got registry.AgentTemplate
	json.NewDecoder(recGet.Body).Decode(&got)
	if got.Status != registry.TemplateStatusDisabled {
		t.Fatalf("get disabled worker status: got %q, want disabled", got.Status)
	}
}

func TestDeleteTemplateGuarded(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
	seedWorker(t, mux)

	del := func(agentType string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("DELETE", "/v1/templates/"+agentType, nil))
		return rec
	}

	// 409 while active (must be disabled first).
	if rec := del("worker"); rec.Code != http.StatusConflict {
		t.Fatalf("delete active: got %d, want 409", rec.Code)
	}

	// Disable, then 204.
	body, _ := json.Marshal(map[string]string{"status": registry.TemplateStatusDisabled})
	recP := httptest.NewRecorder()
	mux.ServeHTTP(recP, httptest.NewRequest("PATCH", "/v1/templates/worker", bytes.NewReader(body)))
	if recP.Code != http.StatusOK {
		t.Fatalf("disable worker: got %d, want 200", recP.Code)
	}
	if rec := del("worker"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete disabled: got %d, want 204", rec.Code)
	}

	// 404 on a second delete (now gone) and on an unknown type.
	if rec := del("worker"); rec.Code != http.StatusNotFound {
		t.Fatalf("delete gone: got %d, want 404", rec.Code)
	}
	if rec := del("nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: got %d, want 404", rec.Code)
	}
}

// globalRelationTemplate carries one tenant-referencing relation and one
// tenant-independent relation, exercising the skip-for-global behavior.
func globalRelationTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "worker", Version: "1.0.0", Status: "active",
		Runtime: registry.RuntimeSpec{Image: "agent-go-worker:latest"},
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
			{Resource: "platform:global", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	}
}

// registerWorker drives a self-registration with the given tenant id and
// returns the recording mock so callers can assert which tuples were written.
func registerWorker(t *testing.T, tpl registry.AgentTemplate, agentID, tenantID string) *spicedb.Mock {
	t.Helper()
	s := newStore()
	s.putTemplate(tpl)
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/" + agentID}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	payload := map[string]string{"agentType": tpl.AgentType, "userId": "user-1"}
	if tenantID != "" {
		payload["tenantId"] = tenantID
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-svid")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("self-register: got %d, want 201", rec.Code)
	}
	return sdb
}

func TestRegister_TenantRelationWrittenWhenTenanted(t *testing.T) {
	sdb := registerWorker(t, workerTemplate(), "agent-tenanted", "acme")
	if ok, _ := sdb.CheckPermission(t.Context(), "tenant:acme", "work_on", "agent:agent-tenanted"); !ok {
		t.Fatal("expected tenant:acme tuple written for tenanted agent")
	}
}

func TestRegister_TenantRelationSkippedWhenGlobal(t *testing.T) {
	sdb := registerWorker(t, workerTemplate(), "agent-global", "")
	// The tenant-referencing relation must be skipped: no "tenant:" tuple
	// (and crucially no malformed "tenant:#..." tuple) should be written.
	if ok, _ := sdb.CheckPermission(t.Context(), "tenant:", "work_on", "agent:agent-global"); ok {
		t.Fatal("expected tenant relation skipped for global agent, but a tenant: tuple was written")
	}
}

func TestRegister_NonTenantRelationAlwaysWritten(t *testing.T) {
	// Present: the non-tenant relation is written alongside the tenant one.
	tenanted := registerWorker(t, globalRelationTemplate(), "agent-t", "acme")
	if ok, _ := tenanted.CheckPermission(t.Context(), "platform:global", "work_on", "agent:agent-t"); !ok {
		t.Fatal("expected non-tenant relation written for tenanted agent")
	}
	if ok, _ := tenanted.CheckPermission(t.Context(), "tenant:acme", "work_on", "agent:agent-t"); !ok {
		t.Fatal("expected tenant relation written for tenanted agent")
	}

	// Absent: the non-tenant relation is still written; the tenant one is skipped.
	global := registerWorker(t, globalRelationTemplate(), "agent-g", "")
	if ok, _ := global.CheckPermission(t.Context(), "platform:global", "work_on", "agent:agent-g"); !ok {
		t.Fatal("expected non-tenant relation written for global agent")
	}
	if ok, _ := global.CheckPermission(t.Context(), "tenant:", "work_on", "agent:agent-g"); ok {
		t.Fatal("expected tenant relation skipped for global agent")
	}
}

func TestSpawnPolicy(t *testing.T) {
	s := newStore()
	s.putTemplate(registry.AgentTemplate{
		AgentType: "parent-agent", Version: "1.0.0", Status: "active",
		Delegation: registry.DelegationPolicy{AllowedChildTypes: []string{"child-agent"}},
	})
	s.registerAgent(registry.AgentRecord{AgentID: "parent-1", AgentType: "parent-agent", TenantID: "tenant-1"})
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	check := func(parentID, childType string) registry.SpawnDecision {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/spawn-policy?parentId="+parentID+"&childType="+childType, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("spawn-policy: got %d, want 200", rec.Code)
		}
		var d registry.SpawnDecision
		json.NewDecoder(rec.Body).Decode(&d)
		return d
	}

	if d := check("parent-1", "child-agent"); !d.Allowed {
		t.Fatalf("expected allowed for listed child, got %+v", d)
	}
	// Deny-by-default: a child type not in allowedChildTypes is rejected.
	if d := check("parent-1", "other-agent"); d.Allowed {
		t.Fatalf("expected denied for unlisted child, got %+v", d)
	}
	// Unknown parent is denied.
	if d := check("ghost", "child-agent"); d.Allowed {
		t.Fatalf("expected denied for unknown parent, got %+v", d)
	}
}

// TestSpawnPolicy_MaxDepth verifies a positive template maxDepth caps total
// chain length: a self-spawning agent type may grow the chain up to maxDepth
// agents, and the spawn that would exceed it is denied.
func TestSpawnPolicy_MaxDepth(t *testing.T) {
	s := newStore()
	s.putTemplate(registry.AgentTemplate{
		AgentType: "chain-worker", Version: "1.0.0", Status: "active",
		Delegation: registry.DelegationPolicy{AllowedChildTypes: []string{"chain-worker"}, MaxDepth: 4},
	})
	// Chain of 4: root (depth 1) -> a (2) -> b (3) -> c (4).
	for _, n := range []struct{ id, parent string }{{"root", ""}, {"a", "root"}, {"b", "a"}, {"c", "b"}} {
		s.registerAgent(registry.AgentRecord{AgentID: n.id, AgentType: "chain-worker", TenantID: "tenant-1", ParentID: n.parent})
	}
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	check := func(parentID string) registry.SpawnDecision {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/spawn-policy?parentId="+parentID+"&childType=chain-worker", nil))
		var d registry.SpawnDecision
		json.NewDecoder(rec.Body).Decode(&d)
		return d
	}

	// b is at depth 3; its child would be depth 4 == maxDepth -> allowed.
	if d := check("b"); !d.Allowed {
		t.Fatalf("depth 4 should be allowed, got %+v", d)
	}
	// c is at depth 4; its child would be depth 5 > maxDepth -> denied.
	if d := check("c"); d.Allowed {
		t.Fatalf("depth 5 should be denied by maxDepth, got %+v", d)
	}
}

func TestAgentSelfRegistration(t *testing.T) {
	s := newStore()
	s.putTemplate(workerTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/agent-test"}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

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
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

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
	// The completion handler now derives the resource types to clean from the
	// agent's template, so the type must be registered.
	s.putTemplate(workerTemplate())
	s.registerAgent(registry.AgentRecord{AgentID: "agent-test", AgentType: "worker", TenantID: "tenant-1", Status: "active"})
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(t.Context(), "tenant:tenant-1", "agent", "agent:agent-test")
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

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

func TestPostTemplate_RejectsSchemaMismatch(t *testing.T) {
	s := newStore() // seeded with the embedded default schema (tenant/agent)
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	// "project:" is not a definition in the default schema → must be rejected
	// before the template is stored.
	body, _ := json.Marshal(projectRelationTemplate())
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/templates", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for schema-mismatched template, got %d", rec.Code)
	}
	// And it must not have been stored.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/templates/proj-worker", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("rejected template should not be stored, got %d", rec2.Code)
	}
}

func TestGetSchema(t *testing.T) {
	s := newStore()
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/schema", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get schema: got %d, want 200", rec.Code)
	}
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["source"] != "default" || resp["version"] != "v1" {
		t.Fatalf("unexpected schema meta: %+v", resp)
	}
	if !strings.Contains(resp["schema"], "definition tenant") {
		t.Fatalf("schema body missing tenant definition: %q", resp["schema"])
	}
}

// projectRelationTemplate uses a non-"tenant" resource type, proving cleanup is
// no longer hardcoded to "tenant" (Phase 1: the delete path derives resource
// types from the template).
func projectRelationTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "proj-worker", Version: "1.0.0", Status: "active",
		Runtime: registry.RuntimeSpec{Image: "agent-go-worker:latest"},
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "project:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	}
}

func TestRevoke_CleansNonTenantResourceType(t *testing.T) {
	s := newStore()
	s.putTemplate(projectRelationTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/proj-1"}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	body, _ := json.Marshal(map[string]string{"agentType": "proj-worker", "userId": "u1", "tenantId": "acme"})
	req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-svid")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: got %d, want 201", rec.Code)
	}
	if ok, _ := sdb.CheckPermission(t.Context(), "project:acme", "work_on", "agent:proj-1"); !ok {
		t.Fatal("expected project:acme tuple written on register")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/proj-1/revoke", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200", rec.Code)
	}
	// Phase 5a: revoke denies via the enabled tuple, so work_on is now false...
	if ok, _ := sdb.CheckPermission(t.Context(), "project:acme", "work_on", "agent:proj-1"); ok {
		t.Fatal("expected work_on denied after revoke")
	}
	// ...but the project:acme template relation itself survives (only enabled
	// toggled). Resume re-enables, and work_on returns — which is only possible
	// if the non-"tenant" project relation was never deleted by revoke.
	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/agents/proj-1/resume", nil))
	if ok, _ := sdb.CheckPermission(t.Context(), "project:acme", "work_on", "agent:proj-1"); !ok {
		t.Fatal("expected work_on restored after resume (project relation must survive revoke)")
	}
}

// TestResumeAfterTemplateDeleted is the regression test for the re-derivation
// drift fixed in Phase 5a: resume re-enables via the single enabled tuple and
// no longer reads the template, so it works even if the template is gone.
func TestResumeAfterTemplateDeleted(t *testing.T) {
	s := newStore()
	s.putTemplate(workerTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/w-1"}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	body, _ := json.Marshal(map[string]string{"agentType": "worker", "userId": "u1", "tenantId": "acme"})
	req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-svid")
	if rec := httptest.NewRecorder(); true {
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("register: got %d", rec.Code)
		}
	}
	work := func() bool {
		ok, _ := sdb.CheckPermission(t.Context(), "tenant:acme", "work_on", "agent:w-1")
		return ok
	}
	if !work() {
		t.Fatal("expected work_on after register")
	}

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/agents/w-1/revoke", nil))
	if work() {
		t.Fatal("expected work_on denied after revoke")
	}

	// Delete the template entirely, then resume — must still re-enable.
	delete(s.templates, "worker")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/w-1/resume", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d", rec.Code)
	}
	if !work() {
		t.Fatal("resume must restore work_on without depending on the template")
	}
}

func TestCompletion_CleansNonTenantResourceType(t *testing.T) {
	s := newStore()
	s.putTemplate(projectRelationTemplate())
	s.registerAgent(registry.AgentRecord{AgentID: "proj-2", AgentType: "proj-worker", TenantID: "acme", Status: "active"})
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(t.Context(), "project:acme", "agent", "agent:proj-2")
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	patchBody, _ := json.Marshal(map[string]string{"status": "completed"})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("PATCH", "/v1/agents/proj-2", bytes.NewReader(patchBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: got %d, want 200", rec.Code)
	}
	if ok, _ := sdb.CheckPermission(t.Context(), "project:acme", "work_on", "agent:proj-2"); ok {
		t.Fatal("expected project:acme tuple deleted on completion")
	}
}

// --- Phase 5b: registry-native consent broker -----------------------------

func consentBrokerMux(t *testing.T) (*store, http.Handler) {
	t.Helper()
	s := newStore()
	return s, buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())
}

func postJSON(t *testing.T, mux http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, rd))
	return rec
}

func TestConsentBroker_CreateApproveProducesRecord(t *testing.T) {
	s, mux := consentBrokerMux(t)

	rec := postJSON(t, mux, "POST", "/v1/consent-requests", map[string]any{
		"userId": "u1", "parentType": "parent", "childType": "child", "scopes": []string{"a", "b"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201", rec.Code)
	}
	var cr registry.ConsentRequest
	json.NewDecoder(rec.Body).Decode(&cr)
	if cr.Status != registry.ConsentPending {
		t.Fatalf("expected pending, got %q", cr.Status)
	}

	// Approve → request approved AND a covering ConsentRecord exists.
	rec2 := postJSON(t, mux, "POST", "/v1/consent-requests/"+cr.ID+"/approve", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("approve: got %d", rec2.Code)
	}
	var approved registry.ConsentRequest
	json.NewDecoder(rec2.Body).Decode(&approved)
	if approved.Status != registry.ConsentApproved {
		t.Fatalf("expected approved, got %q", approved.Status)
	}
	if _, ok := s.findConsent("u1", "parent", "child"); !ok {
		t.Fatal("approve must upsert a ConsentRecord")
	}
	// And GET /v1/consents/check now grants the approved scopes.
	chk := postJSON(t, mux, "GET", "/v1/consents/check?userId=u1&parentType=parent&childType=child&scope=a&scope=b", nil)
	var d registry.ConsentDecision
	json.NewDecoder(chk.Body).Decode(&d)
	if !d.Granted {
		t.Fatalf("expected consent check granted after approve, got %+v", d)
	}
}

func TestConsentBroker_IdempotentOpenRequest(t *testing.T) {
	_, mux := consentBrokerMux(t)
	payload := map[string]any{"userId": "u1", "parentType": "p", "childType": "c", "scopes": []string{"x"}}

	r1 := postJSON(t, mux, "POST", "/v1/consent-requests", payload)
	var c1 registry.ConsentRequest
	json.NewDecoder(r1.Body).Decode(&c1)

	r2 := postJSON(t, mux, "POST", "/v1/consent-requests", payload)
	if r2.Code != http.StatusOK { // 200, not 201 — existing open request returned
		t.Fatalf("second create: got %d, want 200 (existing open request)", r2.Code)
	}
	var c2 registry.ConsentRequest
	json.NewDecoder(r2.Body).Decode(&c2)
	if c1.ID != c2.ID {
		t.Fatalf("expected same open request id, got %q and %q", c1.ID, c2.ID)
	}
}

func TestConsentBroker_ShortCircuitWhenCovered(t *testing.T) {
	s, mux := consentBrokerMux(t)
	// Pre-existing consent covering the scopes.
	s.upsertConsent(registry.ConsentRecord{UserID: "u1", ParentType: "p", ChildType: "c", Scopes: []string{"x", "y"}, GrantedAt: time.Now()})

	rec := postJSON(t, mux, "POST", "/v1/consent-requests", map[string]any{
		"userId": "u1", "parentType": "p", "childType": "c", "scopes": []string{"x"},
	})
	var cr registry.ConsentRequest
	json.NewDecoder(rec.Body).Decode(&cr)
	if cr.Status != registry.ConsentApproved {
		t.Fatalf("expected immediate approval when covered, got %q", cr.Status)
	}
}

func TestConsentBroker_DenyLeavesNoRecord(t *testing.T) {
	s, mux := consentBrokerMux(t)
	rec := postJSON(t, mux, "POST", "/v1/consent-requests", map[string]any{
		"userId": "u1", "parentType": "p", "childType": "c", "scopes": []string{"x"},
	})
	var cr registry.ConsentRequest
	json.NewDecoder(rec.Body).Decode(&cr)

	rec2 := postJSON(t, mux, "POST", "/v1/consent-requests/"+cr.ID+"/deny", nil)
	var denied registry.ConsentRequest
	json.NewDecoder(rec2.Body).Decode(&denied)
	if denied.Status != registry.ConsentDenied {
		t.Fatalf("expected denied, got %q", denied.Status)
	}
	if _, ok := s.findConsent("u1", "p", "c"); ok {
		t.Fatal("deny must not create a ConsentRecord")
	}
}

// TestConsentBroker_PerAgentRequestsAndSweep verifies that two agents waiting on
// the SAME edge each get their OWN consent request (so the dashboard can
// correlate a prompt to a specific pending agent), and that approving one
// sweeps the other to approved via the shared edge grant.
func TestConsentBroker_PerAgentRequestsAndSweep(t *testing.T) {
	_, mux := consentBrokerMux(t)
	mk := func(agentID string) registry.ConsentRequest {
		rec := postJSON(t, mux, "POST", "/v1/consent-requests", map[string]any{
			"userId": "u1", "parentType": "p", "childType": "c", "agentId": agentID, "scopes": []string{"x"},
		})
		var cr registry.ConsentRequest
		json.NewDecoder(rec.Body).Decode(&cr)
		return cr
	}
	a := mk("agent-a")
	b := mk("agent-b")
	if a.ID == b.ID {
		t.Fatal("two agents on the same edge must get distinct consent requests")
	}
	if a.AgentID != "agent-a" || b.AgentID != "agent-b" {
		t.Fatalf("agentId not preserved: %q %q", a.AgentID, b.AgentID)
	}

	// Approving agent-a's request must sweep agent-b's to approved (same edge).
	postJSON(t, mux, "POST", "/v1/consent-requests/"+a.ID+"/approve", nil)
	got, _ := s2GetConsentRequest(t, mux, b.ID)
	if got.Status != registry.ConsentApproved {
		t.Fatalf("expected agent-b swept to approved, got %q", got.Status)
	}
}

func s2GetConsentRequest(t *testing.T, mux http.Handler, id string) (registry.ConsentRequest, int) {
	t.Helper()
	rec := postJSON(t, mux, "GET", "/v1/consent-requests/"+id, nil)
	var cr registry.ConsentRequest
	json.NewDecoder(rec.Body).Decode(&cr)
	return cr, rec.Code
}

func TestConsentBroker_ApproveScopedToUser(t *testing.T) {
	s, mux := consentBrokerMux(t)
	rec := postJSON(t, mux, "POST", "/v1/consent-requests", map[string]any{
		"userId": "u1", "parentType": "p", "childType": "c", "scopes": []string{"x"},
	})
	var cr registry.ConsentRequest
	json.NewDecoder(rec.Body).Decode(&cr)

	// A different user must not be able to approve u1's request.
	other := postJSON(t, mux, "POST", "/v1/consent-requests/"+cr.ID+"/approve?userId=u2", nil)
	if other.Code != http.StatusNotFound {
		t.Fatalf("cross-user approve: got %d, want 404", other.Code)
	}
	if _, ok := s.findConsent("u1", "p", "c"); ok {
		t.Fatal("cross-user approve must not create a ConsentRecord")
	}

	// The owner can.
	ok := postJSON(t, mux, "POST", "/v1/consent-requests/"+cr.ID+"/approve?userId=u1", nil)
	if ok.Code != http.StatusOK {
		t.Fatalf("owner approve: got %d, want 200", ok.Code)
	}
}

func TestConsentBroker_ListPending(t *testing.T) {
	_, mux := consentBrokerMux(t)
	postJSON(t, mux, "POST", "/v1/consent-requests", map[string]any{"userId": "u1", "parentType": "p", "childType": "c", "scopes": []string{"x"}})

	rec := postJSON(t, mux, "GET", "/v1/consent-requests?userId=u1&status=pending", nil)
	var list []registry.ConsentRequest
	json.NewDecoder(rec.Body).Decode(&list)
	if len(list) != 1 || list[0].Status != registry.ConsentPending {
		t.Fatalf("expected one pending request, got %+v", list)
	}
}

func TestPostAndGetEvents(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	payload, _ := json.Marshal(map[string]string{"msg": "hello"})
	evt := events.Event{
		Source:  events.SourceAgent,
		Type:    "task_started",
		Payload: json.RawMessage(payload),
	}
	body, _ := json.Marshal(evt)

	postReq := httptest.NewRequest("POST", "/v1/agents/agent-1/events", bytes.NewReader(body))
	postReq.Header.Set("Content-Type", "application/json")
	postRec := httptest.NewRecorder()
	mux.ServeHTTP(postRec, postReq)

	if postRec.Code != http.StatusCreated {
		t.Fatalf("post event: got %d, want 201", postRec.Code)
	}
	var stored events.Event
	if err := json.NewDecoder(postRec.Body).Decode(&stored); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if stored.ID == "" {
		t.Fatal("expected ID to be assigned")
	}
	if stored.Timestamp.IsZero() {
		t.Fatal("expected Timestamp to be assigned")
	}
	if stored.Type != "task_started" {
		t.Fatalf("expected type task_started, got %q", stored.Type)
	}

	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, httptest.NewRequest("GET", "/v1/agents/agent-1/events", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get events: got %d, want 200", getRec.Code)
	}
	var list []events.Event
	if err := json.NewDecoder(getRec.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 event, got %d", len(list))
	}
	if list[0].ID != stored.ID {
		t.Fatalf("event ID mismatch: %q vs %q", list[0].ID, stored.ID)
	}
}

func TestGetEvents_EmptyReturnsArray(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/unknown-agent/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get events: got %d, want 200", rec.Code)
	}
	// Must decode as array, not null
	var list []events.Event
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list == nil {
		t.Fatal("expected empty array, got nil")
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 events, got %d", len(list))
	}
}

func TestListAgents(t *testing.T) {
	s := newStore()
	s.registerAgent(registry.AgentRecord{AgentID: "agent-a", AgentType: "worker", Status: "active"})
	s.registerAgent(registry.AgentRecord{AgentID: "agent-b", AgentType: "worker", Status: "active"})
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list agents: got %d, want 200", rec.Code)
	}
	var agents []registry.AgentRecord
	if err := json.NewDecoder(rec.Body).Decode(&agents); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(agents))
	}
	ids := map[string]bool{}
	for _, a := range agents {
		ids[a.AgentID] = true
	}
	if !ids["agent-a"] || !ids["agent-b"] {
		t.Fatalf("missing expected agent IDs, got %v", ids)
	}
}

func parentTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "parent-agent", Version: "1.0.0", Status: "active",
		Delegation: registry.DelegationPolicy{
			AllowedChildTypes: []string{"child-agent"},
			GrantableScopes:   []string{"sample-api-b:read"},
			MaxDepth:          3,
		},
	}
}

func decodeDelegation(t *testing.T, mux *http.ServeMux, query string) struct {
	Allowed         bool     `json:"allowed"`
	GrantableScopes []string `json:"grantableScopes"`
	MaxDepth        int      `json:"maxDepth"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/delegation-policy"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("delegation-policy: got %d, want 200", rec.Code)
	}
	var resp struct {
		Allowed         bool     `json:"allowed"`
		GrantableScopes []string `json:"grantableScopes"`
		MaxDepth        int      `json:"maxDepth"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestDelegationPolicy_AllowedChild(t *testing.T) {
	s := newStore()
	s.putTemplate(parentTemplate())
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	resp := decodeDelegation(t, mux, "?parentType=parent-agent&childType=child-agent")
	if !resp.Allowed {
		t.Fatal("expected allowed:true for child-agent")
	}
	if len(resp.GrantableScopes) != 1 || resp.GrantableScopes[0] != "sample-api-b:read" {
		t.Fatalf("unexpected grantableScopes: %v", resp.GrantableScopes)
	}
	if resp.MaxDepth != 3 {
		t.Fatalf("expected maxDepth 3, got %d", resp.MaxDepth)
	}
}

func TestDelegationPolicy_DisallowedChild(t *testing.T) {
	s := newStore()
	s.putTemplate(parentTemplate())
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	resp := decodeDelegation(t, mux, "?parentType=parent-agent&childType=worker")
	if resp.Allowed {
		t.Fatal("expected allowed:false for disallowed child type")
	}
	if resp.GrantableScopes == nil {
		t.Fatal("expected empty array, got nil")
	}
	if len(resp.GrantableScopes) != 0 || resp.MaxDepth != 0 {
		t.Fatalf("expected empty scopes and maxDepth 0, got %v / %d", resp.GrantableScopes, resp.MaxDepth)
	}
}

func TestDelegationPolicy_MissingParentTemplate(t *testing.T) {
	s := newStore()
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	resp := decodeDelegation(t, mux, "?parentType=does-not-exist&childType=child-agent")
	if resp.Allowed {
		t.Fatal("expected allowed:false for missing parent template")
	}
	if resp.GrantableScopes == nil || len(resp.GrantableScopes) != 0 {
		t.Fatalf("expected empty array, got %v", resp.GrantableScopes)
	}
	if resp.MaxDepth != 0 {
		t.Fatalf("expected maxDepth 0, got %d", resp.MaxDepth)
	}
}

func TestDelegationPolicy_NoDelegationConfig(t *testing.T) {
	s := newStore()
	s.putTemplate(workerTemplate()) // no delegation block
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	resp := decodeDelegation(t, mux, "?parentType=worker&childType=child-agent")
	if resp.Allowed {
		t.Fatal("expected allowed:false when template has no delegation config")
	}
}

type chainResp struct {
	Chain []struct {
		AgentID   string `json:"agentId"`
		AgentType string `json:"agentType"`
		Status    string `json:"status"`
		ParentID  string `json:"parentId"`
	} `json:"chain"`
}

func TestAgentChain_MultiLevel(t *testing.T) {
	s := newStore()
	s.registerAgent(registry.AgentRecord{AgentID: "agent-root", AgentType: "parent-agent", Status: "active"})
	s.registerAgent(registry.AgentRecord{AgentID: "agent-mid", AgentType: "parent-agent", Status: "active", ParentID: "agent-root"})
	s.registerAgent(registry.AgentRecord{AgentID: "agent-leaf", AgentType: "child-agent", Status: "active", ParentID: "agent-mid"})
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/agent-leaf/chain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("chain: got %d, want 200", rec.Code)
	}
	var resp chainResp
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Chain) != 3 {
		t.Fatalf("expected chain of 3, got %d", len(resp.Chain))
	}
	wantIDs := []string{"agent-leaf", "agent-mid", "agent-root"}
	for i, id := range wantIDs {
		if resp.Chain[i].AgentID != id {
			t.Fatalf("chain[%d]: got %q, want %q", i, resp.Chain[i].AgentID, id)
		}
	}
	if resp.Chain[2].ParentID != "" {
		t.Fatalf("expected root parentId empty, got %q", resp.Chain[2].ParentID)
	}
}

func TestAgentChain_MissingParentStops(t *testing.T) {
	s := newStore()
	// parent record absent — chain should include only the resolvable node.
	s.registerAgent(registry.AgentRecord{AgentID: "agent-orphan", AgentType: "child-agent", Status: "active", ParentID: "agent-gone"})
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/agent-orphan/chain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("chain: got %d, want 200", rec.Code)
	}
	var resp chainResp
	json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Chain) != 1 {
		t.Fatalf("expected chain of 1, got %d", len(resp.Chain))
	}
}

func TestAgentChain_CycleGuard(t *testing.T) {
	s := newStore()
	s.registerAgent(registry.AgentRecord{AgentID: "agent-a", AgentType: "x", Status: "active", ParentID: "agent-b"})
	s.registerAgent(registry.AgentRecord{AgentID: "agent-b", AgentType: "x", Status: "active", ParentID: "agent-a"})
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/agent-a/chain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("chain: got %d, want 200", rec.Code)
	}
	var resp chainResp
	json.NewDecoder(rec.Body).Decode(&resp)
	// Cycle guard must terminate; each agent appears at most once.
	if len(resp.Chain) != 2 {
		t.Fatalf("expected chain of 2 with cycle guard, got %d", len(resp.Chain))
	}
}

func TestAgentChain_UnknownAgent404(t *testing.T) {
	s := newStore()
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/nope/chain", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("chain unknown: got %d, want 404", rec.Code)
	}
}

func TestSelfRegistrationEmitsEvents(t *testing.T) {
	s := newStore()
	s.putTemplate(workerTemplate())
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/agent-emit"}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

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

	getRec := httptest.NewRecorder()
	mux.ServeHTTP(getRec, httptest.NewRequest("GET", "/v1/agents/agent-emit/events", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("get events: got %d, want 200", getRec.Code)
	}
	var evts []events.Event
	if err := json.NewDecoder(getRec.Body).Decode(&evts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evts) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(evts))
	}
	types := map[string]bool{}
	for _, e := range evts {
		types[e.Type] = true
	}
	if !types["registry_record_created"] {
		t.Fatal("missing registry_record_created event")
	}
	if !types["spicedb_relations_written"] {
		t.Fatal("missing spicedb_relations_written event")
	}
}

func TestRevokeResume_AuthZ(t *testing.T) {
	s := newStore()
	s.putTemplate(registry.AgentTemplate{
		AgentType: "worker",
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	})
	s.registerAgent(registry.AgentRecord{AgentID: "agent-test", AgentType: "worker", TenantID: "tenant-1", Status: "active"})
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(t.Context(), "tenant:tenant-1", "agent", "agent:agent-test")
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

	// revoke: status -> revoked, tuple removed
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/agent-test/revoke", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200", rec.Code)
	}
	if s.getAgent("agent-test").Status != "revoked" {
		t.Fatal("status not revoked")
	}
	if ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:agent-test"); ok {
		t.Fatal("expected SpiceDB tuple removed on revoke")
	}

	// resume: status -> active, tuple restored from template
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/agents/agent-test/resume", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200", rec2.Code)
	}
	if s.getAgent("agent-test").Status != "active" {
		t.Fatal("status not active after resume")
	}
	if ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:agent-test"); !ok {
		t.Fatal("expected SpiceDB tuple restored on resume")
	}

	// resume of an already-active agent -> 200, no-op (empty resumed list)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/agents/agent-test/resume", nil))
	if rec3.Code != http.StatusOK {
		t.Fatalf("resume already-active: got %d, want 200", rec3.Code)
	}
	var body struct {
		Resumed []string `json:"resumed"`
	}
	json.NewDecoder(rec3.Body).Decode(&body)
	if len(body.Resumed) != 0 {
		t.Fatalf("resume already-active should be a no-op, resumed %v", body.Resumed)
	}

	// revoke unknown agent -> 404
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, httptest.NewRequest("POST", "/v1/agents/nope/revoke", nil))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: got %d, want 404", rec4.Code)
	}
}

// TestRevokeResume_Cascade verifies that revoking a mid-chain agent cascades to
// its whole descendant subtree while leaving ancestors and unrelated agents
// untouched, and that resume restores exactly that subtree.
func TestRevokeResume_Cascade(t *testing.T) {
	s := newStore()
	s.putTemplate(registry.AgentTemplate{
		AgentType: "worker",
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	})
	sdb := spicedb.NewMock()

	// Chain: root -> a -> b -> c (4 deep). Plus an unrelated top-level agent.
	chain := []struct{ id, parent string }{
		{"root", ""}, {"a", "root"}, {"b", "a"}, {"c", "b"}, {"other", ""},
	}
	for _, n := range chain {
		s.registerAgent(registry.AgentRecord{AgentID: n.id, AgentType: "worker", TenantID: "tenant-1", Status: "active", ParentID: n.parent})
		sdb.WriteRelationship(t.Context(), "tenant:tenant-1", "agent", "agent:"+n.id)
		sdb.WriteRelationship(t.Context(), "agent:"+n.id, "enabled", "agent:"+n.id)
	}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	has := func(id string) bool {
		ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:"+id)
		return ok
	}

	// Revoke "a": expect a, b, c revoked; root and other untouched.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/a/revoke", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200", rec.Code)
	}
	for _, id := range []string{"a", "b", "c"} {
		if s.getAgent(id).Status != "revoked" {
			t.Fatalf("%s: expected status revoked, got %q", id, s.getAgent(id).Status)
		}
		if has(id) {
			t.Fatalf("%s: expected SpiceDB tuple removed", id)
		}
	}
	for _, id := range []string{"root", "other"} {
		if s.getAgent(id).Status != "active" {
			t.Fatalf("%s: expected untouched (active), got %q", id, s.getAgent(id).Status)
		}
		if !has(id) {
			t.Fatalf("%s: expected SpiceDB tuple intact", id)
		}
	}

	// A node that exited independently must not be resurrected by resume.
	s.updateAgent("b", "failed")

	// Resume "a": a and c restored; b stays failed (and gets no tuple back).
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/agents/a/resume", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200", rec2.Code)
	}
	for _, id := range []string{"a", "c"} {
		if s.getAgent(id).Status != "active" {
			t.Fatalf("%s: expected active after resume, got %q", id, s.getAgent(id).Status)
		}
		if !has(id) {
			t.Fatalf("%s: expected SpiceDB tuple restored", id)
		}
	}
	if s.getAgent("b").Status != "failed" {
		t.Fatalf("b: resume must not resurrect a failed agent, got %q", s.getAgent("b").Status)
	}
	if has("b") {
		t.Fatal("b: failed agent must not get its tuple back")
	}
}

// TestRevokeCascade_PreservesTerminalDescendant verifies that a descendant which
// already exited BEFORE the cascade keeps its terminal status: revoke must not
// clobber it to "revoked" (which would let a later resume resurrect it), and it
// must not appear in the revoked list.
func TestRevokeCascade_PreservesTerminalDescendant(t *testing.T) {
	s := newStore()
	s.putTemplate(registry.AgentTemplate{
		AgentType: "worker",
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	})
	sdb := spicedb.NewMock()

	// Chain root -> a -> c, where c has already completed before the revoke.
	for _, n := range []struct{ id, parent, status string }{
		{"root", "", "active"}, {"a", "root", "active"}, {"c", "a", "completed"},
	} {
		s.registerAgent(registry.AgentRecord{AgentID: n.id, AgentType: "worker", TenantID: "tenant-1", Status: n.status, ParentID: n.parent})
		if n.status == "active" {
			sdb.WriteRelationship(t.Context(), "tenant:tenant-1", "agent", "agent:"+n.id)
			sdb.WriteRelationship(t.Context(), "agent:"+n.id, "enabled", "agent:"+n.id)
		}
	}
	mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())
	has := func(id string) bool {
		ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:"+id)
		return ok
	}

	// Revoke root: only root and a are active, so only they are revoked. c stays
	// completed and is absent from the revoked list.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/root/revoke", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke: got %d, want 200", rec.Code)
	}
	var body struct {
		Revoked []string `json:"revoked"`
	}
	json.NewDecoder(rec.Body).Decode(&body)
	if got := strings.Join(body.Revoked, ","); got != "root,a" {
		t.Fatalf("revoked list = %q, want \"root,a\" (terminal c excluded)", got)
	}
	if s.getAgent("c").Status != "completed" {
		t.Fatalf("c: terminal status clobbered, got %q", s.getAgent("c").Status)
	}

	// Resume root must not resurrect the completed node.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/agents/root/resume", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200", rec2.Code)
	}
	if s.getAgent("c").Status != "completed" {
		t.Fatalf("c: resume resurrected a completed agent, got %q", s.getAgent("c").Status)
	}
	if has("c") {
		t.Fatal("c: completed agent must not have a SpiceDB tuple")
	}
}

// consentParentTemplate gates child-agent spawns behind user consent with a
// long TTL; ttl is overridable per test.
func consentParentTemplate(ttl string) registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "parent-agent", Version: "1.0.0", Status: "active",
		Delegation: registry.DelegationPolicy{
			AllowedChildTypes: []string{"child-agent"},
			ChildPolicies: map[string]registry.ChildSpawnPolicy{
				"child-agent": {RequireUserConsent: true, ConsentTTL: ttl},
			},
		},
	}
}

func TestConsentLifecycle(t *testing.T) {
	s := newStore()
	s.putTemplate(consentParentTemplate("720h"))
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	check := func(scopes ...string) registry.ConsentDecision {
		t.Helper()
		url := "/v1/consents/check?userId=alice&parentType=parent-agent&childType=child-agent"
		for _, sc := range scopes {
			url += "&scope=" + sc
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", url, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("consent check: got %d, want 200", rec.Code)
		}
		var d registry.ConsentDecision
		json.NewDecoder(rec.Body).Decode(&d)
		return d
	}
	grant := func() registry.ConsentRecord {
		t.Helper()
		body := `{"userId":"alice","parentType":"parent-agent","childType":"child-agent","scopes":["openid","sample-api-b:read"]}`
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents", strings.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("record consent: got %d, want 201", rec.Code)
		}
		var cr registry.ConsentRecord
		json.NewDecoder(rec.Body).Decode(&cr)
		return cr
	}

	// No consent on record yet.
	if d := check("openid"); d.Granted {
		t.Fatalf("expected no consent yet, got %+v", d)
	}

	cr := grant()
	if cr.ID == "" || cr.ExpiresAt == nil {
		t.Fatalf("grant should set id and TTL-derived expiry, got %+v", cr)
	}

	// Covered scopes pass; escalation re-prompts.
	if d := check("sample-api-b:read"); !d.Granted {
		t.Fatalf("expected granted, got %+v", d)
	}
	if d := check("sample-api-a:write"); d.Granted {
		t.Fatalf("expected scope escalation denial, got %+v", d)
	}

	// Listing is per user.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/consents?userId=alice", nil))
	var list []registry.ConsentRecord
	json.NewDecoder(rec.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("expected 1 consent for alice, got %d", len(list))
	}

	// Revoke forces re-consent; a fresh grant re-approves under the same id.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents/"+cr.ID+"/revoke", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke consent: got %d, want 204", rec.Code)
	}
	if d := check("openid"); d.Granted || d.Reason != "consent revoked" {
		t.Fatalf("expected revoked denial, got %+v", d)
	}
	if regrant := grant(); regrant.ID != cr.ID || regrant.Revoked {
		t.Fatalf("re-grant should reuse id and clear revocation, got %+v", regrant)
	}
	if d := check("openid"); !d.Granted {
		t.Fatalf("expected granted after re-grant, got %+v", d)
	}

	// Revoking an unknown id is a 404.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents/ghost/revoke", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("revoke unknown: got %d, want 404", rec.Code)
	}
}

func TestConsentExpiryDerivedFromTemplateTTL(t *testing.T) {
	s := newStore()
	s.putTemplate(consentParentTemplate("1ns"))
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	body := `{"userId":"alice","parentType":"parent-agent","childType":"child-agent","scopes":["openid"]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("record consent: got %d, want 201", rec.Code)
	}

	time.Sleep(time.Millisecond) // ensure the 1ns TTL has elapsed
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET",
		"/v1/consents/check?userId=alice&parentType=parent-agent&childType=child-agent&scope=openid", nil))
	var d registry.ConsentDecision
	json.NewDecoder(rec.Body).Decode(&d)
	if d.Granted || d.Reason != "consent expired" {
		t.Fatalf("expected expired denial, got %+v", d)
	}
}

// TestSpawnPolicy_ConsentRequired verifies the per-child consent gate is
// surfaced on the spawn decision (and only for the flagged child type).
func TestSpawnPolicy_ConsentRequired(t *testing.T) {
	s := newStore()
	tpl := consentParentTemplate("720h")
	tpl.Delegation.AllowedChildTypes = []string{"child-agent", "other-agent"}
	s.putTemplate(tpl)
	s.registerAgent(registry.AgentRecord{AgentID: "parent-1", AgentType: "parent-agent", TenantID: "tenant-1"})
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	check := func(childType string) registry.SpawnDecision {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/spawn-policy?parentId=parent-1&childType="+childType, nil))
		var d registry.SpawnDecision
		json.NewDecoder(rec.Body).Decode(&d)
		return d
	}

	if d := check("child-agent"); !d.Allowed || !d.ConsentRequired {
		t.Fatalf("expected allowed+consentRequired for flagged child, got %+v", d)
	}
	if d := check("other-agent"); !d.Allowed || d.ConsentRequired {
		t.Fatalf("expected allowed without consent for unflagged child, got %+v", d)
	}
}

// TestRegister_RefusesToResurrectDroppedAuthority guards against the
// restart-resurrection bug: a native sidecar container restarts independently
// of its pod and re-registers on boot. Registration must never flip a record
// whose SpiceDB authority was deliberately dropped (failed/completed/revoked)
// back to active, nor rewrite its tuples.
func TestRegister_RefusesToResurrectDroppedAuthority(t *testing.T) {
	for _, status := range []string{"failed", "completed", "revoked"} {
		t.Run(status, func(t *testing.T) {
			s := newStore()
			s.putTemplate(workerTemplate())
			sdb := spicedb.NewMock()
			validator := &spiffe.MockSVIDValidator{SpiffeID: "spiffe://cluster.local/agent/agent-dead"}
			mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())

			s.registerAgent(registry.AgentRecord{
				AgentID: "agent-dead", AgentType: "worker", UserID: "user-1", Status: status,
			})

			body, _ := json.Marshal(map[string]string{"agentType": "worker", "userId": "user-1", "tenantId": "acme"})
			req := httptest.NewRequest("POST", "/v1/agents", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-svid")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("re-register %s agent: got %d, want 409", status, rec.Code)
			}
			if got := s.getAgent("agent-dead").Status; got != status {
				t.Fatalf("status overwritten: got %q, want %q", got, status)
			}
			if ok, _ := sdb.CheckPermission(t.Context(), "tenant:acme", "work_on", "agent:agent-dead"); ok {
				t.Fatal("SpiceDB authority must not be restored by re-registration")
			}
		})
	}
}

// TestConsentRevoke_ScopedToOwner: a userId on the revoke call restricts it to
// that user's own consents — another user's (guessed) id 404s and the record
// stays live.
func TestConsentRevoke_ScopedToOwner(t *testing.T) {
	s := newStore()
	s.putTemplate(consentParentTemplate("720h"))
	mux := buildMux(s, spicedb.NewMock(), registrant.NewSpiffeVerifier(&spiffe.MockSVIDValidator{}), controlplane.AllowAll())

	body := `{"userId":"alice","parentType":"parent-agent","childType":"child-agent","scopes":["openid"]}`
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("record consent: got %d, want 201", rec.Code)
	}
	var cr registry.ConsentRecord
	json.NewDecoder(rec.Body).Decode(&cr)

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents/"+cr.ID+"/revoke?userId=mallory", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-user revoke: got %d, want 404", rec.Code)
	}
	if got := s.listConsents("alice"); len(got) != 1 || got[0].Revoked {
		t.Fatalf("alice's consent must survive a cross-user revoke, got %+v", got)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/consents/"+cr.ID+"/revoke?userId=alice", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner revoke: got %d, want 204", rec.Code)
	}
	if got := s.listConsents("alice"); len(got) != 1 || !got[0].Revoked {
		t.Fatalf("owner revoke must mark the record revoked, got %+v", got)
	}
}

// subtreeIDs drives GET /v1/agents/{id}/subtree and returns the decoded id list
// plus the HTTP status code.
func subtreeIDs(t *testing.T, mux http.Handler, id string) ([]string, int) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/agents/"+id+"/subtree", nil))
	if rec.Code != http.StatusOK {
		return nil, rec.Code
	}
	var resp struct {
		Subtree []string `json:"subtree"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode subtree response: %v", err)
	}
	return resp.Subtree, rec.Code
}

// TestSubtreeEndpoint exercises GET /v1/agents/{id}/subtree, the read side a
// cascading delete consumes: the named agent plus every descendant reachable
// through ParentID edges, root first.
func TestSubtreeEndpoint(t *testing.T) {
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}

	// Linear chain: root -> a -> b -> c.
	t.Run("linear chain", func(t *testing.T) {
		s := newStore()
		mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
		seed := func(id, parent string) {
			s.registerAgent(registry.AgentRecord{AgentID: id, AgentType: "chain-worker", TenantID: "tenant-1", ParentID: parent})
		}
		for _, n := range []struct{ id, parent string }{{"root", ""}, {"a", "root"}, {"b", "a"}, {"c", "b"}} {
			seed(n.id, n.parent)
		}

		if got, _ := subtreeIDs(t, mux, "root"); !slices.Equal(got, []string{"root", "a", "b", "c"}) {
			t.Fatalf("subtree(root) = %v, want [root a b c]", got)
		}
		if got, _ := subtreeIDs(t, mux, "a"); !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Fatalf("subtree(a) = %v, want [a b c]", got)
		}
		if got, _ := subtreeIDs(t, mux, "c"); !slices.Equal(got, []string{"c"}) {
			t.Fatalf("subtree(c) = %v, want [c]", got)
		}
	})

	// Branching tree: root -> {x, y}, x -> x1. BFS sibling order between x and y
	// is not guaranteed, so compare as a set — but the root must come first.
	t.Run("branching tree", func(t *testing.T) {
		s := newStore()
		mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
		seed := func(id, parent string) {
			s.registerAgent(registry.AgentRecord{AgentID: id, AgentType: "chain-worker", TenantID: "tenant-1", ParentID: parent})
		}
		for _, n := range []struct{ id, parent string }{{"root", ""}, {"x", "root"}, {"y", "root"}, {"x1", "x"}} {
			seed(n.id, n.parent)
		}

		got, _ := subtreeIDs(t, mux, "root")
		if len(got) == 0 || got[0] != "root" {
			t.Fatalf("subtree(root) must have root first, got %v", got)
		}
		sorted := append([]string(nil), got...)
		slices.Sort(sorted)
		if !slices.Equal(sorted, []string{"root", "x", "x1", "y"}) {
			t.Fatalf("subtree(root) set = %v, want {root x y x1}", got)
		}
	})

	// Unknown id -> 404.
	t.Run("unknown id", func(t *testing.T) {
		s := newStore()
		mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
		if _, code := subtreeIDs(t, mux, "nope"); code != http.StatusNotFound {
			t.Fatalf("subtree(unknown) status = %d, want 404", code)
		}
	})

	// A descendant whose status is terminal is still part of the delete set.
	t.Run("terminal descendant still included", func(t *testing.T) {
		s := newStore()
		mux := buildMux(s, sdb, registrant.NewSpiffeVerifier(validator), controlplane.AllowAll())
		seed := func(id, parent string) {
			s.registerAgent(registry.AgentRecord{AgentID: id, AgentType: "chain-worker", TenantID: "tenant-1", ParentID: parent})
		}
		seed("root", "")
		seed("dead", "root")
		if _, err := s.UpdateAgentStatus(context.Background(), "dead", "completed"); err != nil {
			t.Fatalf("set terminal status: %v", err)
		}

		got, _ := subtreeIDs(t, mux, "root")
		if !slices.Contains(got, "dead") {
			t.Fatalf("terminal descendant must still appear in subtree, got %v", got)
		}
	})
}
