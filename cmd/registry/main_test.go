// cmd/registry/main_test.go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agent-platform/poc/internal/events"
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

// globalRelationTemplate carries one tenant-referencing relation and one
// tenant-independent relation, exercising the skip-for-global behavior.
func globalRelationTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "worker", Version: "1.0.0", Status: "active",
		Runtime: registry.RuntimeSpec{Image: "agent-agent:latest"},
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
	mux := buildMux(s, sdb, validator)

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
	mux := buildMux(s, sdb, validator)

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

func TestPostAndGetEvents(t *testing.T) {
	s := newStore()
	sdb := spicedb.NewMock()
	validator := &spiffe.MockSVIDValidator{}
	mux := buildMux(s, sdb, validator)

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
	mux := buildMux(s, sdb, validator)

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
	mux := buildMux(s, sdb, validator)

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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
	mux := buildMux(s, spicedb.NewMock(), &spiffe.MockSVIDValidator{})

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

func TestSuspendResume_AuthZ(t *testing.T) {
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
	mux := buildMux(s, sdb, validator)

	// suspend: status -> suspended, tuple removed
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/agent-test/suspend", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("suspend: got %d, want 200", rec.Code)
	}
	if s.getAgent("agent-test").Status != "suspended" {
		t.Fatal("status not suspended")
	}
	if ok, _ := sdb.CheckPermission(t.Context(), "tenant:tenant-1", "work_on", "agent:agent-test"); ok {
		t.Fatal("expected SpiceDB tuple removed on suspend")
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

	// resume of a non-suspended agent -> 409
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("POST", "/v1/agents/agent-test/resume", nil))
	if rec3.Code != http.StatusConflict {
		t.Fatalf("resume non-suspended: got %d, want 409", rec3.Code)
	}

	// suspend unknown agent -> 404
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, httptest.NewRequest("POST", "/v1/agents/nope/suspend", nil))
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("suspend unknown: got %d, want 404", rec4.Code)
	}
}
