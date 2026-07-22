package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fakeclient "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// spawnRegMock serves the registry surface POST /spawn touches: the template, the
// org (optionally with an ACTIVE substrate), and preregister capture.
func spawnRegMock(t *testing.T, substrateActive bool, captured *registry.AgentRecord) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/preregister":
			if captured != nil {
				_ = json.NewDecoder(r.Body).Decode(captured)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(registry.AgentTemplate{
				AgentType: "worker", Version: "1.0.0", Status: "active",
				Runtime: registry.RuntimeSpec{Lifecycle: "long-lived"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/orgs/acme":
			org := registry.Organization{OrgID: "acme", Name: "Acme"}
			if substrateActive {
				org.Substrate = &registry.Substrate{Status: registry.SubstrateActive, CertFingerprint: "fp"}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(org)
		default:
			http.NotFound(w, r)
		}
	}))
}

// driveSpawn posts POST /spawn as an org member (seeding the SpiceDB member tuple
// requireMember/orgMemberCheck need), returning the recorder and the k8s client so
// the test can assert whether a local AgentWorkload CR was created.
func driveSpawn(t *testing.T, mock *httptest.Server) (*httptest.ResponseRecorder, client.Client) {
	t.Helper()
	sdb := spicedb.NewMock()
	enc := spicedb.EncodeObjectID
	sdb.WriteRelationship(context.Background(), "organization:"+enc("acme"), "member", "user:"+enc("viewer"))

	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	claims := memberClaims("viewer", "acme")
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mock.URL,
		fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")

	req := httptest.NewRequest("POST", "/spawn", strings.NewReader(`{"agentType":"worker","task":"go"}`))
	req.Header.Set("Authorization", "Bearer member-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, fakeClient
}

func listWorkloads(t *testing.T, c client.Client) []agentv1alpha1.AgentWorkload {
	t.Helper()
	var list agentv1alpha1.AgentWorkloadList
	if err := c.List(context.Background(), &list); err != nil {
		t.Fatalf("list workloads: %v", err)
	}
	return list.Items
}

// Active substrate ⇒ NO local CR, and a substrate-placed pending record.
func TestSpawn_ActiveSubstrate_SkipsCRAndPlacesSubstrate(t *testing.T) {
	var pre registry.AgentRecord
	mock := spawnRegMock(t, true, &pre)
	defer mock.Close()

	rec, k8s := driveSpawn(t, mock)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("spawn: got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	if items := listWorkloads(t, k8s); len(items) != 0 {
		t.Fatalf("active substrate must NOT create a local AgentWorkload CR, got %d", len(items))
	}
	if pre.Placement != registry.PlacementSubstrate {
		t.Fatalf("preregistered Placement = %q, want substrate", pre.Placement)
	}
	if pre.OrgID != "acme" {
		t.Fatalf("preregistered OrgID = %q, want acme", pre.OrgID)
	}
	var resp SpawnResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.HasPrefix(resp.WorkloadName, "agent-") {
		t.Fatalf("workloadName = %q, want agent-<id>", resp.WorkloadName)
	}
}

// No substrate ⇒ today's path: a local CR IS created, record is NOT substrate-placed.
func TestSpawn_NoSubstrate_PlatformHostedRegression(t *testing.T) {
	var pre registry.AgentRecord
	mock := spawnRegMock(t, false, &pre)
	defer mock.Close()

	rec, k8s := driveSpawn(t, mock)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("spawn: got %d, want 202 (body: %s)", rec.Code, rec.Body.String())
	}
	items := listWorkloads(t, k8s)
	if len(items) != 1 {
		t.Fatalf("no substrate ⇒ exactly one local AgentWorkload CR, got %d", len(items))
	}
	if pre.Placement == registry.PlacementSubstrate {
		t.Fatal("no substrate ⇒ record must not be substrate-placed")
	}
}
