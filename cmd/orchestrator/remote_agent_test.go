// cmd/orchestrator/remote_agent_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fakeclient "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
	"github.com/spawnly/platform/internal/tokenvalidator"
)

// remoteRegMock builds a mock registry for the remote-agent registration path.
// It serves GET /v1/templates/{type} (from tpl), GET /v1/orgs/{id} (an org with
// or without an active anchor per anchored), and captures the preregistered
// record. Any preregister is accepted with 201.
func remoteRegMock(t *testing.T, tpl registry.AgentTemplate, anchored bool, captured *registry.AgentRecord) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/preregister":
			if captured != nil {
				json.NewDecoder(r.Body).Decode(captured)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/templates/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tpl)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/orgs/"):
			org := registry.Organization{OrgID: strings.TrimPrefix(r.URL.Path, "/v1/orgs/"), Name: "Org"}
			if anchored {
				org.Anchor = &registry.AttestationAnchor{Type: "sts", AWSAccountID: "111122223333", RoleArnPattern: "arn:aws:sts::111122223333:assumed-role/x/*", Status: registry.AnchorActive}
			} else {
				org.Anchor = &registry.AttestationAnchor{Type: "sts", AWSAccountID: "111122223333", Status: registry.AnchorPending}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(org)
		default:
			http.NotFound(w, r)
		}
	}))
}

// memberClaims returns a single-org member token for org, with the write scope.
func memberClaims(user, org string) tokenvalidator.Claims {
	return tokenvalidator.Claims{User: user, OrgID: org, Audience: []string{"orchestrator"}, Scopes: []string{"orchestrator:write"}}
}

// driveRegisterRemote drives POST /v1/orgs/{pathOrg}/agents with claims and body,
// seeding a member tuple for claims.OrgID so requireOrgMember admits a member.
func driveRegisterRemote(t *testing.T, claims tokenvalidator.Claims, pathOrg, body string, mock *httptest.Server) *httptest.ResponseRecorder {
	t.Helper()
	sdb := spicedb.NewMock()
	if claims.OrgID != "" {
		enc := spicedb.EncodeObjectID
		user := strings.TrimPrefix(claims.User, "user:")
		sdb.WriteRelationship(context.Background(), "organization:"+enc(claims.OrgID), "member", "user:"+enc(user))
	}
	fakeClient := fake.NewClientBuilder().WithScheme(newScheme()).Build()
	mux := buildMux(fakeClient, fakeclient.NewSimpleClientset(), sdb, mock.URL,
		fakeValidator{claims: claims}, "orchestrator", "orchestrator:spawn", "orchestrator:read", "orchestrator:write")

	req := httptest.NewRequest("POST", "/v1/orgs/"+pathOrg+"/agents", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer member-token")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func globalTpl() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "worker", Version: "1.0.0", Status: "active",
		Runtime: registry.RuntimeSpec{Lifecycle: "long-lived"},
	}
}

// TestRegisterRemote_MemberSucceeds: a member of the anchored org registers a
// remote agent; the record is preregistered remote, org-scoped, owned by the sub.
func TestRegisterRemote_MemberSucceeds(t *testing.T) {
	var pre registry.AgentRecord
	mock := remoteRegMock(t, globalTpl(), true, &pre)
	defer mock.Close()

	rec := driveRegisterRemote(t, memberClaims("viewer", "acme"), "acme", `{"agentType":"worker"}`, mock)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (body: %s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		AgentID string `json:"agentId"`
	}
	json.NewDecoder(rec.Body).Decode(&resp)
	if !strings.HasPrefix(resp.AgentID, "remote-") {
		t.Errorf("agentId = %q, want remote-<shortID>", resp.AgentID)
	}
	if pre.AgentID != resp.AgentID {
		t.Errorf("preregistered agentId %q != response %q", pre.AgentID, resp.AgentID)
	}
	if pre.Placement != registry.PlacementRemote {
		t.Errorf("Placement = %q, want remote", pre.Placement)
	}
	if pre.OrgID != "acme" {
		t.Errorf("OrgID = %q, want acme (from the path, server-set)", pre.OrgID)
	}
	if pre.UserID != "viewer" {
		t.Errorf("UserID = %q, want viewer (the token sub)", pre.UserID)
	}
}

// TestRegisterRemote_MemberOfOtherOrg404: a member of globex cannot register into
// acme — requireOrgMember 404s a cross-org caller (no existence oracle).
func TestRegisterRemote_MemberOfOtherOrg404(t *testing.T) {
	mock := remoteRegMock(t, globalTpl(), true, nil)
	defer mock.Close()

	// Claim names globex; path is acme → cross-org, must 404.
	rec := driveRegisterRemote(t, memberClaims("bob", "globex"), "acme", `{"agentType":"worker"}`, mock)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org register: got %d, want 404 (body: %s)", rec.Code, rec.Body.String())
	}
}

// TestRegisterRemote_UnanchoredOrg409: registering into an org with no ACTIVE
// anchor is refused 409 — the agent could never authenticate.
func TestRegisterRemote_UnanchoredOrg409(t *testing.T) {
	mock := remoteRegMock(t, globalTpl(), false, nil)
	defer mock.Close()

	rec := driveRegisterRemote(t, memberClaims("viewer", "acme"), "acme", `{"agentType":"worker"}`, mock)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unanchored org: got %d, want 409 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "active anchor") {
		t.Errorf("409 body = %q, want it to mention the missing active anchor", rec.Body.String())
	}
}

// TestRegisterRemote_TemplateGates covers the /spawn-equivalent template gates.
func TestRegisterRemote_TemplateGates(t *testing.T) {
	t.Run("other-org template reads as unknown 400", func(t *testing.T) {
		tpl := globalTpl()
		tpl.OrgID = "globex" // scoped to a DIFFERENT org
		mock := remoteRegMock(t, tpl, true, nil)
		defer mock.Close()
		rec := driveRegisterRemote(t, memberClaims("viewer", "acme"), "acme", `{"agentType":"worker"}`, mock)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("other-org template: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("disabled template 409", func(t *testing.T) {
		tpl := globalTpl()
		tpl.Status = registry.TemplateStatusDisabled
		mock := remoteRegMock(t, tpl, true, nil)
		defer mock.Close()
		rec := driveRegisterRemote(t, memberClaims("viewer", "acme"), "acme", `{"agentType":"worker"}`, mock)
		if rec.Code != http.StatusConflict {
			t.Fatalf("disabled template: got %d, want 409 (body: %s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("requiresTenant without tenantId 400", func(t *testing.T) {
		tpl := globalTpl()
		tpl.RequiresTenant = true
		mock := remoteRegMock(t, tpl, true, nil)
		defer mock.Close()
		rec := driveRegisterRemote(t, memberClaims("viewer", "acme"), "acme", `{"agentType":"worker"}`, mock)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("requiresTenant: got %d, want 400 (body: %s)", rec.Code, rec.Body.String())
		}
	})
}
