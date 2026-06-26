// cmd/orchestrator/main_test.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

// fakeValidator is a tokenvalidator.TokenValidator stub for the agent spawn path:
// it returns canned claims (the agent's ACT-AS token, already validated) or a
// fixed error, with no real signature/issuer checking.
type fakeValidator struct {
	claims tokenvalidator.Claims
	err    error
}

func (f fakeValidator) ValidateAccessToken(_ context.Context, _ string) (tokenvalidator.Claims, error) {
	if f.err != nil {
		return tokenvalidator.Claims{}, f.err
	}
	return f.claims, nil
}

// agentClaims returns canned claims for a valid orchestrator spawn token: human
// owner user:<user>, acting agent <parent>, audience "orchestrator", scope
// "orchestrator:spawn".
func agentClaims(user, parent string) tokenvalidator.Claims {
	return tokenvalidator.Claims{
		User:            "user:" + user,
		ActingAgentName: parent,
		Audience:        []string{"orchestrator"},
		Scopes:          []string{"orchestrator:spawn"},
	}
}

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)
	return s
}

// defaultMockRegistry returns an httptest.Server that responds to:
//   - POST /v1/agents/preregister → 201
//   - POST /v1/agents/*/events   → 201
//   - GET  /v1/agents            → []
//   - GET  /v1/agents/parent-1   → a parent record (userId=alice, tenantId=t1)
//   - GET  /v1/templates/{type}  → stub template with lifecycle ""
//   - GET  /v1/spawn-policy       → allowed iff childType == "child-agent"
//
// The /v1/agents/parent-1 record backs the agent spawn path: the orchestrator
// GetAgents the parent to derive the child's userId/tenantId from its record.
func defaultMockRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/preregister":
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents/parent-1":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(registry.AgentRecord{
				AgentID:   "parent-1",
				AgentType: "parent-agent",
				UserID:    "alice",
				TenantID:  "t1",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/spawn-policy":
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Query().Get("childType") == "child-agent" {
				w.Write([]byte(`{"allowed":true,"reason":""}`))
			} else {
				w.Write([]byte(`{"allowed":false,"reason":"not permitted"}`))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"agentType":"worker","version":"1.0.0","status":"active","meta":{"displayName":"Worker","description":"Test"},"runtimeSpec":{"image":"agent-go-worker:latest","lifecycle":"","resources":{"cpuLimits":"","memoryLimits":""},"envDefaults":{}},"authzTemplate":{"spiceDbRelations":[]}}`))
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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

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

func TestSpawnWithAllowedParentSucceeds(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	// Agent path: parentId comes from the verified token (parent-1), not the body.
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: agentClaims("alice", "parent-1")}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "child-agent", // allowed by the mock spawn-policy
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")
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
	if list.Items[0].Spec.ParentID != "parent-1" {
		t.Fatalf("unexpected parentId: %q", list.Items[0].Spec.ParentID)
	}
}

func TestSpawnWithDisallowedParentDenied(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	// Agent path: parentId comes from the verified token (parent-1), not the body.
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: agentClaims("alice", "parent-1")}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "forbidden-type", // denied by the mock spawn-policy
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
	// No workload should have been created.
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected 0 AgentWorkloads after denied spawn, got %d", len(list.Items))
	}
}

// TestSpawnAgentPathIgnoresSpoofedBody asserts that on the agent path the child's
// userId/tenantId/parentId are derived from the verified token + parent record,
// never the body — so a compromised agent can't forge who it spawns for or where.
func TestSpawnAgentPathIgnoresSpoofedBody(t *testing.T) {
	mockReg := defaultMockRegistry(t) // serves GET /v1/agents/parent-1 → alice/t1
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: agentClaims("alice", "parent-1")}, "orchestrator", "orchestrator:spawn", "")

	// child-agent is allowed by the mock spawn-policy. The body lies about who it
	// spawns for and where in the tree.
	body, _ := json.Marshal(SpawnRequest{
		AgentType: "child-agent",
		UserID:    "mallory",
		TenantID:  "evil",
		ParentID:  "someone-else",
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}

	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("expected 1 AgentWorkload, got %d", len(list.Items))
	}
	spec := list.Items[0].Spec
	if spec.UserID != "alice" {
		t.Errorf("userId: got %q, want alice (body spoof must be ignored)", spec.UserID)
	}
	if spec.TenantID != "t1" {
		t.Errorf("tenantId: got %q, want t1 (body spoof must be ignored)", spec.TenantID)
	}
	if spec.ParentID != "parent-1" {
		t.Errorf("parentId: got %q, want parent-1 (must come from token)", spec.ParentID)
	}
}

// TestSpawnAgentPathInvalidTokenRejected asserts a failed token verification on
// the agent path yields 401 and creates no workload.
func TestSpawnAgentPathInvalidTokenRejected(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{err: errors.New("bad")}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{AgentType: "child-agent"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer bogus")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected 0 AgentWorkloads, got %d", len(list.Items))
	}
}

// TestSpawnAgentPathAudienceMismatchRejected asserts that a validly-signed token
// minted for a DIFFERENT resource server (e.g. sample-api-a) cannot be replayed
// at /spawn: its aud doesn't contain "orchestrator", so the spawn is rejected
// with 401 and no workload is created. This is the cross-audience-replay defense.
func TestSpawnAgentPathAudienceMismatchRejected(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	// Valid token (no error), correct scope, but wrong audience.
	claims := tokenvalidator.Claims{
		User:            "user:alice",
		ActingAgentName: "parent-1",
		Audience:        []string{"sample-api-a"},
		Scopes:          []string{"orchestrator:spawn"},
	}
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{AgentType: "child-agent"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-aud")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 (wrong-audience token must not be replayable)", rec.Code)
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected 0 AgentWorkloads, got %d", len(list.Items))
	}
}

// TestSpawnAgentPathDelegationTokenRejected asserts a delegation-only token
// (token_use=delegation) cannot be used to spawn — 401, before aud/scope.
func TestSpawnAgentPathDelegationTokenRejected(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	claims := agentClaims("alice", "parent-1")
	claims.TokenUse = "delegation"
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{AgentType: "child-agent"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer delegation")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 (delegation token must not spawn)", rec.Code)
	}
}

// TestSpawnAgentPathMissingScopeRejected asserts a token with the right audience
// but lacking the spawn scope is 403 (distinct from the 401 audience/auth cases).
func TestSpawnAgentPathMissingScopeRejected(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	claims := agentClaims("alice", "parent-1")
	claims.Scopes = []string{"sample-api-a:read"} // right aud, wrong scope
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{AgentType: "child-agent"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer no-scope")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (missing spawn scope)", rec.Code)
	}
}

// TestSpawnAgentPathUnknownParentRejected asserts that a valid token whose acting
// agent isn't a known registry record is 403 — the parent must exist to derive
// the child's userId/tenantId.
func TestSpawnAgentPathUnknownParentRejected(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	// defaultMockRegistry only knows parent-1; a different acting agent 404s.
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{claims: agentClaims("alice", "ghost-parent")}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{AgentType: "child-agent"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer unknown-parent")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (unknown parent agent)", rec.Code)
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected no workload, got %d", len(list.Items))
	}
}

// TestSpawnDashboardPathRejectsParentID asserts the dashboard path (no auth
// header) rejects a body parentId with 400 — human spawns are top-level only.
func TestSpawnDashboardPathRejectsParentID(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "worker",
		UserID:    "user-1",
		ParentID:  "parent-1",
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected 0 AgentWorkloads, got %d", len(list.Items))
	}
}

// TestSpawnDashboardPathControlPlaneToken asserts that when a control-plane token
// is configured, the dashboard path (no Authorization header) is authenticated by
// the SEPARATE X-Control-Plane-Token header: the right token → 202, and the same
// request with the header omitted → 401 (so an attacker can't reach the open
// dashboard path by simply dropping the agent-path JWT).
func TestSpawnDashboardPathControlPlaneToken(t *testing.T) {
	const token = "s3cret"

	newMux := func(fakeClient client.Client) *http.ServeMux {
		sdb := spicedb.NewMock()
		return buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, "", fakeValidator{}, "orchestrator", "orchestrator:spawn", token)
	}
	makeReq := func(withToken bool) *http.Request {
		body, _ := json.Marshal(SpawnRequest{AgentType: "worker", UserID: "user-1"})
		req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if withToken {
			req.Header.Set("X-Control-Plane-Token", token)
		}
		return req
	}

	// With the correct X-Control-Plane-Token → 202 and a workload is created.
	t.Run("valid token accepted", func(t *testing.T) {
		mockReg := defaultMockRegistry(t)
		defer mockReg.Close()
		fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
		sdb := spicedb.NewMock()
		mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", token)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, makeReq(true))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
		}
		var list agentv1alpha1.AgentWorkloadList
		fakeClient.List(context.Background(), &list)
		if len(list.Items) != 1 {
			t.Fatalf("expected 1 AgentWorkload, got %d", len(list.Items))
		}
	})

	// Same request WITHOUT the X-Control-Plane-Token header → 401, no workload.
	t.Run("missing token rejected", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
		mux := newMux(fakeClient)

		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, makeReq(false))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("got %d, want 401", rec.Code)
		}
		var list agentv1alpha1.AgentWorkloadList
		fakeClient.List(context.Background(), &list)
		if len(list.Items) != 0 {
			t.Fatalf("expected 0 AgentWorkloads, got %d", len(list.Items))
		}
	})
}

func TestSpawnMissingRequiredFields(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

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

// mockRegistryWithTemplate returns an httptest.Server like defaultMockRegistry,
// except its GET /v1/templates/{type} response sets requiresTenant to the given
// value, letting tests exercise the tenant-presence guard.
func mockRegistryWithTemplate(t *testing.T, requiresTenant bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			tpl := registry.AgentTemplate{
				AgentType:      "worker",
				Version:        "1.0.0",
				Status:         "active",
				Meta:           registry.TemplateMeta{DisplayName: "Worker", Description: "Test"},
				Runtime:        registry.RuntimeSpec{Image: "agent-go-worker:latest", EnvDefaults: map[string]string{}},
				AuthZ:          registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{}},
				RequiresTenant: requiresTenant,
			}
			json.NewEncoder(w).Encode(tpl)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSpawnTenantPresenceGuard(t *testing.T) {
	tests := []struct {
		name           string
		requiresTenant bool
		userID         string
		tenantID       string
		wantStatus     int
		wantWorkload   bool   // a single AgentWorkload should exist
		wantTenantID   string // expected Spec.TenantID when wantWorkload
	}{
		{
			name:           "requiresTenant with tenant accepted",
			requiresTenant: true,
			userID:         "user-1",
			tenantID:       "acme",
			wantStatus:     http.StatusAccepted,
			wantWorkload:   true,
			wantTenantID:   "acme",
		},
		{
			name:           "requiresTenant without tenant rejected",
			requiresTenant: true,
			userID:         "user-1",
			tenantID:       "",
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "global template with tenant is tenanted",
			requiresTenant: false,
			userID:         "user-1",
			tenantID:       "acme",
			wantStatus:     http.StatusAccepted,
			wantWorkload:   true,
			wantTenantID:   "acme",
		},
		{
			name:           "global template without tenant is global",
			requiresTenant: false,
			userID:         "user-1",
			tenantID:       "",
			wantStatus:     http.StatusAccepted,
			wantWorkload:   true,
			wantTenantID:   "",
		},
		{
			name:           "missing userId rejected even when tenant present",
			requiresTenant: false,
			userID:         "",
			tenantID:       "acme",
			wantStatus:     http.StatusBadRequest,
		},
		{
			name:           "missing userId rejected when requiresTenant",
			requiresTenant: true,
			userID:         "",
			tenantID:       "acme",
			wantStatus:     http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockReg := mockRegistryWithTemplate(t, tc.requiresTenant)
			defer mockReg.Close()

			fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
			sdb := spicedb.NewMock()
			mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

			body, _ := json.Marshal(SpawnRequest{
				AgentType: "worker",
				UserID:    tc.userID,
				TenantID:  tc.tenantID,
			})
			req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("got %d, want %d (body: %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			var list agentv1alpha1.AgentWorkloadList
			fakeClient.List(context.Background(), &list)
			if tc.wantWorkload {
				if len(list.Items) != 1 {
					t.Fatalf("expected 1 AgentWorkload, got %d", len(list.Items))
				}
				if list.Items[0].Spec.TenantID != tc.wantTenantID {
					t.Fatalf("unexpected Spec.TenantID: got %q, want %q", list.Items[0].Spec.TenantID, tc.wantTenantID)
				}
			} else if len(list.Items) != 0 {
				t.Fatalf("expected 0 AgentWorkloads, got %d", len(list.Items))
			}
		})
	}
}

// TestSpawnDisabledTemplateRejected asserts the spawn gate rejects a spawn whose
// target template is disabled with 409 and creates no AgentWorkload.
func TestSpawnDisabledTemplateRejected(t *testing.T) {
	mockReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			tpl := registry.AgentTemplate{
				AgentType: "worker",
				Version:   "1.0.0",
				Status:    registry.TemplateStatusDisabled,
				Meta:      registry.TemplateMeta{DisplayName: "Worker", Description: "Test"},
				Runtime:   registry.RuntimeSpec{Image: "agent-go-worker:latest", EnvDefaults: map[string]string{}},
				AuthZ:     registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{}},
			}
			json.NewEncoder(w).Encode(tpl)
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "worker",
		UserID:    "user-1",
		TenantID:  "tenant-1",
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("got %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected 0 AgentWorkloads after disabled-template spawn, got %d", len(list.Items))
	}
}

func TestSpawnRequiresAgentType(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	// A spawn with no agentType is rejected: there is no default agent type.
	body, _ := json.Marshal(map[string]string{"userId": "u1", "tenantId": "t1"})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}

	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected no AgentWorkload created for a typeless spawn, got %d", len(list.Items))
	}
}

func TestSpawnWithTask(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

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

// TestRevokeResumeProxyToRegistry asserts the orchestrator forwards revoke/resume
// to the registry under the matching path and relays its status and JSON body.
func TestRevokeResumeProxyToRegistry(t *testing.T) {
	var gotPaths []string
	mockReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"revoked":["a1","a2"]}`))
	}))
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	for _, action := range []string{"revoke", "resume"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/agents/a1/"+action, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", action, rec.Code)
		}
		want := "POST /v1/agents/a1/" + action
		if len(gotPaths) == 0 || gotPaths[len(gotPaths)-1] != want {
			t.Fatalf("%s: registry saw %v, want last %q", action, gotPaths, want)
		}
		if !strings.Contains(rec.Body.String(), `"revoked"`) {
			t.Fatalf("%s: body not relayed, got %q", action, rec.Body.String())
		}
	}
}

func TestDeleteAgent_NotFound(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("DELETE", "/v1/agents/missing", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestParseLogLines(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		sinceTime string
		want      []logLine
	}{
		{
			name: "splits timestamp and text on first space",
			raw:  "2026-05-28T21:00:01.000000000Z hello world\n",
			want: []logLine{{TS: "2026-05-28T21:00:01.000000000Z", Text: "hello world"}},
		},
		{
			name: "skips empty lines",
			raw:  "2026-05-28T21:00:01.000000000Z a\n\n2026-05-28T21:00:02.000000000Z b\n",
			want: []logLine{
				{TS: "2026-05-28T21:00:01.000000000Z", Text: "a"},
				{TS: "2026-05-28T21:00:02.000000000Z", Text: "b"},
			},
		},
		{
			name: "line with no text yields empty text",
			raw:  "2026-05-28T21:00:01.000000000Z\n",
			want: []logLine{{TS: "2026-05-28T21:00:01.000000000Z", Text: ""}},
		},
		{
			name:      "sinceTime filters strictly after",
			raw:       "2026-05-28T21:00:01.000000000Z a\n2026-05-28T21:00:02.000000000Z b\n2026-05-28T21:00:03.000000000Z c\n",
			sinceTime: "2026-05-28T21:00:02.000000000Z",
			want:      []logLine{{TS: "2026-05-28T21:00:03.000000000Z", Text: "c"}},
		},
		{
			name:      "sinceTime excludes exact match",
			raw:       "2026-05-28T21:00:02.000000000Z b\n",
			sinceTime: "2026-05-28T21:00:02.000000000Z",
			want:      []logLine{},
		},
		{
			name: "empty input yields empty slice",
			raw:  "",
			want: []logLine{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseLogLines(tc.raw, tc.sinceTime)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d lines, want %d: %#v", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("line %d: got %#v, want %#v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLogsInvalidContainerRejected(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("GET", "/v1/agents/agent-1a2b/logs?container=bogus", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestLogsDefaultContainerAndWaitingState(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	// No AgentWorkload and no pod exist. The handler must default the container
	// to "agent", fall back to "{id}-pod" for the pod name, report phase
	// "Pending" (no pod found), and return 200 (never 5xx).
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("GET", "/v1/agents/agent-1a2b/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp logsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Container != "agent" {
		t.Fatalf("default container: got %q, want agent", resp.Container)
	}
	if resp.PodName != "agent-1a2b-pod" {
		t.Fatalf("pod name fallback: got %q, want agent-1a2b-pod", resp.PodName)
	}
	if resp.PodPhase != "Pending" {
		t.Fatalf("pod phase: got %q, want Pending", resp.PodPhase)
	}
	if resp.Complete {
		t.Fatalf("expected complete=false")
	}
}

func TestIsContainerNotReadyErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"waiting to start", errors.New("container \"agent\" in pod \"x\" is waiting to start: ContainerCreating"), true},
		{"ContainerCreating", errors.New("ContainerCreating"), true},
		{"not found", errors.New("pods \"x\" not found"), true},
		{"PodInitializing", errors.New("container \"agent\" is waiting: PodInitializing"), true},
		{"genuine error", errors.New("connection refused"), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContainerNotReadyErr(tc.err); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLogsResolvesPodNameFromStatusAndPhase(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-zzzz", Namespace: "default"},
	}
	aw.Status.PodName = "agent-zzzz-pod"
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(aw).Build()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-zzzz-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	cs := fakeclient.NewSimpleClientset(pod)

	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, cs, sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("GET", "/v1/agents/agent-zzzz/logs", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp logsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.PodName != "agent-zzzz-pod" {
		t.Fatalf("pod name: got %q", resp.PodName)
	}
	if resp.PodPhase != string(corev1.PodSucceeded) {
		t.Fatalf("pod phase: got %q, want Succeeded", resp.PodPhase)
	}
	if !resp.Complete {
		t.Fatalf("expected complete=true for Succeeded phase")
	}
}

// --- Cascade DELETE tests -------------------------------------------------

// cascadeMockRegistry serves GET /v1/agents/{id}/subtree from a fixed map and
// 404s unknown ids, plus the POST /events endpoint the handler may touch. The
// subtree intentionally does NOT shrink across calls — mirroring production,
// where deleting a CR only marks the registry record terminal, not removed.
func cascadeMockRegistry(t *testing.T, subtrees map[string][]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/subtree"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/agents/"), "/subtree")
			nodes, ok := subtrees[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string][]string{"subtree": nodes})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
}

// agentWorkloads builds AgentWorkload CRs (namespace "default") for the given ids.
func agentWorkloads(ids ...string) []client.Object {
	objs := make([]client.Object, 0, len(ids))
	for _, id := range ids {
		aw := &agentv1alpha1.AgentWorkload{}
		aw.Name = id
		aw.Namespace = "default"
		objs = append(objs, aw)
	}
	return objs
}

func TestDeleteCascadeChain(t *testing.T) {
	cascadeSettleDelay = 0
	mockReg := cascadeMockRegistry(t, map[string][]string{
		"root": {"root", "a", "b", "c"},
	})
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(agentWorkloads("root", "a", "b", "c")...).
		Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("DELETE", "/v1/agents/root", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", rec.Code)
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected all CRs deleted, %d remain", len(list.Items))
	}
}

func TestDeleteCascadeLeaf(t *testing.T) {
	cascadeSettleDelay = 0
	mockReg := cascadeMockRegistry(t, map[string][]string{
		"leaf": {"leaf"},
	})
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(agentWorkloads("leaf")...).
		Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("DELETE", "/v1/agents/leaf", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204", rec.Code)
	}
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 0 {
		t.Fatalf("expected leaf CR deleted, %d remain", len(list.Items))
	}
}

func TestDeleteCascadeUnknownRoot(t *testing.T) {
	cascadeSettleDelay = 0
	// Empty map → every id 404s → first-pass-empty → 404.
	mockReg := cascadeMockRegistry(t, map[string][]string{})
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("DELETE", "/v1/agents/ghost", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
}

func TestDeleteCascadeIdempotentAlreadyGone(t *testing.T) {
	cascadeSettleDelay = 0
	// Registry still knows the subtree, but the CRs no longer exist in the
	// cluster. Every Delete returns NotFound → deletedThisPass==0 on pass 0 →
	// converge to 204 with no failures.
	mockReg := cascadeMockRegistry(t, map[string][]string{
		"root": {"root", "a"},
	})
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build() // no objects
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("DELETE", "/v1/agents/root", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204 (idempotent already-gone)", rec.Code)
	}
}

func TestDeleteCascadePartialFailure(t *testing.T) {
	cascadeSettleDelay = 0
	mockReg := cascadeMockRegistry(t, map[string][]string{
		"root": {"root", "a", "b"},
	})
	defer mockReg.Close()

	// Intercept the fake client so deleting "b" returns a non-NotFound error,
	// leaving root/a deletable. The cascade should report 207 with b in failed[].
	fakeClient := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(agentWorkloads("root", "a", "b")...).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if obj.GetName() == "b" {
					return errors.New("simulated delete failure")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL, fakeValidator{}, "orchestrator", "orchestrator:spawn", "")

	req := httptest.NewRequest("DELETE", "/v1/agents/root", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("got %d, want 207", rec.Code)
	}
	var body struct {
		Deleted int      `json:"deleted"`
		Failed  []string `json:"failed"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode 207 body: %v", err)
	}
	if body.Deleted != 2 {
		t.Errorf("deleted = %d, want 2 (root+a)", body.Deleted)
	}
	// b appears exactly once despite being retried across sweeps (de-duped).
	if len(body.Failed) != 1 || body.Failed[0] != "b" {
		t.Errorf("failed = %v, want [b]", body.Failed)
	}
	// b survives; root and a are gone.
	var list agentv1alpha1.AgentWorkloadList
	fakeClient.List(context.Background(), &list)
	if len(list.Items) != 1 || list.Items[0].Name != "b" {
		t.Fatalf("expected only b to remain, got %d items", len(list.Items))
	}
}
