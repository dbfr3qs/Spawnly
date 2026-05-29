// cmd/sample-api/main_test.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-platform/poc/internal/spicedb"
	"github.com/agent-platform/poc/internal/tokenvalidator"
)

const testAudience = "sample-api-a"

func testConfig() apiConfig {
	return apiConfig{audience: testAudience, scopePrefix: testAudience}
}

// claimsFor builds Claims for an acting agent with the given audience/scopes.
func claimsFor(agentSpiffe string, scopes []string) tokenvalidator.Claims {
	return tokenvalidator.Claims{
		User:            "user:user-1",
		ActingAgent:     agentSpiffe,
		ActingAgentName: lastSegment(agentSpiffe),
		Chain:           []string{agentSpiffe},
		Scopes:          scopes,
		Audience:        []string{testAudience},
	}
}

func lastSegment(s string) string {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return s
	}
	return s[i+1:]
}

func TestWorkHandlerAllowed(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-1")

	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-1", []string{"sample-api-a:read"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["status"] != "ok" {
		t.Fatalf("unexpected body: %v", resp)
	}
}

func TestWorkHandlerInvalidToken(t *testing.T) {
	sdb := spicedb.NewMock()
	validator := &tokenvalidator.MockValidator{Err: fmt.Errorf("invalid token")}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer bad-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestWorkHandlerSpiceDBDenied(t *testing.T) {
	sdb := spicedb.NewMock() // no grants
	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-99", []string{"sample-api-a:read"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

// A token minted for the other API (wrong aud) must be rejected with 401.
func TestWorkHandlerWrongAudience(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-1")

	c := claimsFor("spiffe://cluster.local/agent/agent-1", []string{"sample-api-a:read"})
	c.Audience = []string{"sample-api-b"}
	validator := &tokenvalidator.MockValidator{Claims: c}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// A delegation-only token (token_use=delegation) must be rejected with 401.
func TestWorkHandlerDelegationTokenRejected(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-1")

	c := claimsFor("spiffe://cluster.local/agent/agent-1", []string{"sample-api-a:read"})
	c.Audience = []string{"delegation"}
	c.TokenUse = "delegation"
	validator := &tokenvalidator.MockValidator{Claims: c}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

// Missing the required read scope must be 403.
func TestWorkHandlerMissingScope(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-1")

	// Only a write scope present, but GET requires read.
	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-1", []string{"sample-api-a:write"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

func TestTaskHandlerAllowed(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-abc")

	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-abc", []string{"sample-api-a:write"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	body := strings.NewReader(`{"task":"hello"}`)
	req := httptest.NewRequest("POST", "/task", body)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var resp map[string]string
	json.NewDecoder(rec.Body).Decode(&resp)
	if resp["result"] != "echo: hello" {
		t.Fatalf("unexpected result: %v", resp["result"])
	}
	if resp["agentName"] != "agent-abc" {
		t.Fatalf("unexpected agentName: %v", resp["agentName"])
	}
}

// POST /work behaves like POST /task and requires the write scope.
func TestPostWorkHandlerAllowed(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-abc")

	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-abc", []string{"sample-api-a:write"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	body := strings.NewReader(`{"task":"hello"}`)
	req := httptest.NewRequest("POST", "/work", body)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
}

func TestTaskHandlerMissingTenantID(t *testing.T) {
	sdb := spicedb.NewMock()
	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-abc", []string{"sample-api-a:write"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	body := strings.NewReader(`{"task":"hello"}`)
	req := httptest.NewRequest("POST", "/task", body)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	// no X-Tenant-ID

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

func TestTaskHandlerMissingAuth(t *testing.T) {
	sdb := spicedb.NewMock()
	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-abc", []string{"sample-api-a:write"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	body := strings.NewReader(`{"task":"hello"}`)
	req := httptest.NewRequest("POST", "/task", body)
	req.Header.Set("X-Tenant-ID", "tenant-1")
	// no Authorization

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestTaskHandlerInvalidToken(t *testing.T) {
	sdb := spicedb.NewMock()
	validator := &tokenvalidator.MockValidator{Err: fmt.Errorf("invalid token")}
	mux := buildMux(sdb, validator, testConfig())

	body := strings.NewReader(`{"task":"hello"}`)
	req := httptest.NewRequest("POST", "/task", body)
	req.Header.Set("Authorization", "Bearer bad-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
}

func TestTaskHandlerSpiceDBDenied(t *testing.T) {
	sdb := spicedb.NewMock() // no grants
	validator := &tokenvalidator.MockValidator{
		Claims: claimsFor("spiffe://cluster.local/agent/agent-abc", []string{"sample-api-a:write"}),
	}
	mux := buildMux(sdb, validator, testConfig())

	body := strings.NewReader(`{"task":"hello"}`)
	req := httptest.NewRequest("POST", "/task", body)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403", rec.Code)
	}
}

// A two-member chain where the acting (child) agent is authorized but an
// ancestor (parent) has been suspended — its work_on grant removed — must
// cascade to 403, even though the child itself is allowed.
func TestWorkHandlerSuspendedAncestorDenied(t *testing.T) {
	child := "spiffe://cluster.local/agent/tenant-1/user-1/child-agent/agent-child"
	parent := "spiffe://cluster.local/agent/tenant-1/user-1/parent-agent/agent-parent"

	sdb := spicedb.NewMock()
	// child keeps work_on; parent's grant is absent (suspended).
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-child")

	c := claimsFor(child, []string{"sample-api-a:read"})
	c.Chain = []string{child, parent} // outermost (acting) first, ancestor nested
	validator := &tokenvalidator.MockValidator{Claims: c}
	mux := buildMux(sdb, validator, testConfig())

	req := httptest.NewRequest("GET", "/work", nil)
	req.Header.Set("Authorization", "Bearer fake-access-token")
	req.Header.Set("X-Tenant-ID", "tenant-1")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d, want 403 (suspended ancestor should cascade-deny)", rec.Code)
	}
}
