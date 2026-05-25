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

func TestWorkHandlerAllowed(t *testing.T) {
	sdb := spicedb.NewMock()
	sdb.WriteRelationship(context.Background(), "tenant:tenant-1", "agent", "agent:agent-1")

	validator := &tokenvalidator.MockValidator{SpiffeID: "spiffe://cluster.local/agent/agent-1"}
	mux := buildMux(sdb, validator)

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
	mux := buildMux(sdb, validator)

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
	validator := &tokenvalidator.MockValidator{SpiffeID: "spiffe://cluster.local/agent/agent-99"}
	mux := buildMux(sdb, validator)

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

	validator := &tokenvalidator.MockValidator{SpiffeID: "spiffe://cluster.local/agent/agent-abc"}
	mux := buildMux(sdb, validator)

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

func TestTaskHandlerMissingTenantID(t *testing.T) {
	sdb := spicedb.NewMock()
	validator := &tokenvalidator.MockValidator{SpiffeID: "spiffe://cluster.local/agent/agent-abc"}
	mux := buildMux(sdb, validator)

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
	validator := &tokenvalidator.MockValidator{SpiffeID: "spiffe://cluster.local/agent/agent-abc"}
	mux := buildMux(sdb, validator)

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
	mux := buildMux(sdb, validator)

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
	validator := &tokenvalidator.MockValidator{SpiffeID: "spiffe://cluster.local/agent/agent-abc"}
	mux := buildMux(sdb, validator)

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
