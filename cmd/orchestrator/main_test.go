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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/registry"
	"github.com/agent-platform/poc/internal/spicedb"
)

func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	clientgoscheme.AddToScheme(s)
	agentv1alpha1.AddToScheme(s)
	return s
}

// defaultMockRegistry returns an httptest.Server that responds to:
//   - POST /v1/agents/*/events   → 201
//   - GET  /v1/agents            → []
//   - GET  /v1/templates/{type}  → stub template with lifecycle ""
//   - GET  /v1/spawn-policy       → allowed iff childType == "child-agent"
func defaultMockRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/events"):
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "child-agent", // allowed by the mock spawn-policy
		UserID:    "user-1",
		TenantID:  "tenant-1",
		ParentID:  "parent-1",
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
	if list.Items[0].Spec.ParentID != "parent-1" {
		t.Fatalf("unexpected parentId: %q", list.Items[0].Spec.ParentID)
	}
}

func TestSpawnWithDisallowedParentDenied(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

	body, _ := json.Marshal(SpawnRequest{
		AgentType: "forbidden-type", // denied by the mock spawn-policy
		UserID:    "user-1",
		TenantID:  "tenant-1",
		ParentID:  "parent-1",
	})
	req := httptest.NewRequest("POST", "/spawn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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

func TestSpawnMissingRequiredFields(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
			mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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

func TestSpawnDefaultAgentType(t *testing.T) {
	mockReg := defaultMockRegistry(t)
	defer mockReg.Close()

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	sdb := spicedb.NewMock()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mockReg.URL)

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
	mux := buildMux(fakeClient, cs, sdb, mockReg.URL)

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
