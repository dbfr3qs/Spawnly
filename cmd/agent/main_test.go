// cmd/agent/main_test.go
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-platform/poc/internal/registry"
)

type MockFetcher struct {
	Tokens map[string]string // audience → JWT token
	Err    error
}

func (m *MockFetcher) FetchJWT(_ context.Context, audience string) (string, error) {
	if m.Err != nil {
		return "", m.Err
	}
	return m.Tokens[audience], nil
}

// newRegistryHandler returns an http.HandlerFunc that handles POST /v1/agents
// (self-registration) and POST /v1/agents/{id}/events (event emission).
func newRegistryHandler(agentID string, checkAuth bool, authToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/agents" {
			if checkAuth && r.Header.Get("Authorization") != "Bearer "+authToken {
				http.Error(w, "bad auth", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(registry.AgentRecord{AgentID: agentID, Status: "active"})
			return
		}
		// Handle event POST calls: POST /v1/agents/{id}/events
		if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/agents/") {
			w.WriteHeader(http.StatusCreated)
			return
		}
		http.NotFound(w, r)
	}
}

func TestRunAgentSuccess(t *testing.T) {
	regSrv := httptest.NewServer(newRegistryHandler("agent-test", true, "reg-svid"))
	defer regSrv.Close()

	isSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.FormValue("client_assertion") != "is-svid" {
			http.Error(w, "bad assertion", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": "the-access-token"})
	}))
	defer isSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer the-access-token" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer apiSrv.Close()

	fetcher := &MockFetcher{Tokens: map[string]string{
		"registry": "reg-svid",
		isSrv.URL: "is-svid",
	}}
	cfg := AgentConfig{
		TenantID: "tenant-1", UserID: "user-1", AgentType: "worker",
		RegistryURL: regSrv.URL, ISTokenURL: isSrv.URL, SampleAPIURL: apiSrv.URL,
	}
	if err := runAgent(context.Background(), cfg, fetcher); err != nil {
		t.Fatalf("runAgent: %v", err)
	}
}

func TestRunAgentSVIDFetchFails(t *testing.T) {
	fetcher := &MockFetcher{Err: fmt.Errorf("spire unavailable")}
	cfg := AgentConfig{TenantID: "t", UserID: "u", AgentType: "worker",
		RegistryURL: "http://unused", ISTokenURL: "http://unused", SampleAPIURL: "http://unused"}
	if err := runAgent(context.Background(), cfg, fetcher); err == nil {
		t.Fatal("expected error when SVID fetch fails")
	}
}

func TestRunAgentRegistryRejects(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer regSrv.Close()

	fetcher := &MockFetcher{Tokens: map[string]string{"registry": "reg-svid"}}
	cfg := AgentConfig{TenantID: "t", UserID: "u", AgentType: "worker",
		RegistryURL: regSrv.URL, ISTokenURL: "http://unused", SampleAPIURL: "http://unused"}
	if err := runAgent(context.Background(), cfg, fetcher); err == nil {
		t.Fatal("expected error when registry rejects SVID")
	}
}

func TestRunAgentSampleAPIForbidden(t *testing.T) {
	regSrv := httptest.NewServer(newRegistryHandler("agent-test", false, ""))
	defer regSrv.Close()

	isSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": "the-token"})
	}))
	defer isSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer apiSrv.Close()

	fetcher := &MockFetcher{Tokens: map[string]string{"registry": "r", isSrv.URL: "i"}}
	cfg := AgentConfig{TenantID: "t", UserID: "u", AgentType: "worker",
		RegistryURL: regSrv.URL, ISTokenURL: isSrv.URL, SampleAPIURL: apiSrv.URL}
	if err := runAgent(context.Background(), cfg, fetcher); err == nil {
		t.Fatal("expected error on 403 from sample API")
	}
}

func TestDecodeJWTPayload(t *testing.T) {
	payload := map[string]any{"sub": "test"}
	b, _ := json.Marshal(payload)
	encoded := base64.RawURLEncoding.EncodeToString(b)
	token := "header." + encoded + ".sig"

	claims := decodeJWTPayload(token)
	if claims == nil {
		t.Fatal("expected non-nil claims")
	}
	if claims["sub"] != "test" {
		t.Fatalf("expected sub=test, got %v", claims["sub"])
	}
}

func TestDecodeJWTPayloadInvalid(t *testing.T) {
	if decodeJWTPayload("notajwt") != nil {
		t.Fatal("expected nil for non-JWT string")
	}
	if decodeJWTPayload("a.b") != nil {
		t.Fatal("expected nil for two-part string")
	}
}

func TestSelfRegisterReturnsAgentID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/agents" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(registry.AgentRecord{
				AgentID: "agent-abc", AgentType: "worker",
				TenantID: "t1", UserID: "u1", Status: "active",
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	id, err := selfRegister(context.Background(), srv.URL, "svid-token", "worker", "t1", "u1")
	if err != nil {
		t.Fatalf("selfRegister: %v", err)
	}
	if id != "agent-abc" {
		t.Fatalf("expected agent-abc, got %q", id)
	}
}

func TestRunAgentWithTask(t *testing.T) {
	regSrv := httptest.NewServer(newRegistryHandler("agent-test", false, ""))
	defer regSrv.Close()

	isSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": "task-access-token"})
	}))
	defer isSrv.Close()

	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/task" {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"result":    "echo: " + body["task"],
				"task":      body["task"],
				"agentName": "agent-test",
			})
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer apiSrv.Close()

	fetcher := &MockFetcher{Tokens: map[string]string{
		"registry": "reg-svid",
		isSrv.URL: "is-svid",
	}}
	cfg := AgentConfig{
		TenantID: "tenant-1", UserID: "user-1", AgentType: "worker",
		RegistryURL: regSrv.URL, ISTokenURL: isSrv.URL, SampleAPIURL: apiSrv.URL,
		Task: "hello",
	}
	if err := runAgent(context.Background(), cfg, fetcher); err != nil {
		t.Fatalf("runAgent with task: %v", err)
	}
}
