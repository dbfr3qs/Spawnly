// cmd/agent/main_test.go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestRunAgentSuccess(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/agents" {
			if r.Header.Get("Authorization") != "Bearer reg-svid" {
				http.Error(w, "bad auth", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(registry.AgentRecord{AgentID: "agent-test", Status: "active"})
		}
	}))
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
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(registry.AgentRecord{AgentID: "agent-test", Status: "active"})
	}))
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
