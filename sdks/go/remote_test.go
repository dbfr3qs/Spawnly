package spawnly

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterRemoteAgent(t *testing.T) {
	t.Run("posts to the org endpoint with the member token and returns agentId", func(t *testing.T) {
		var gotAuth, gotPath, gotType string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			gotType = body["agentType"]
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"agentId":"remote-abc123"}`))
		}))
		defer srv.Close()

		id, err := RegisterRemoteAgent(context.Background(), RemoteRegistration{
			OrchestratorURL: srv.URL,
			OrgID:           "acme",
			MemberToken:     "member-tok",
			AgentType:       "weather-monitor",
		})
		if err != nil {
			t.Fatalf("RegisterRemoteAgent: %v", err)
		}
		if id != "remote-abc123" {
			t.Errorf("agentId = %q, want remote-abc123", id)
		}
		if gotAuth != "Bearer member-tok" {
			t.Errorf("Authorization = %q", gotAuth)
		}
		if gotPath != "/v1/orgs/acme/agents" {
			t.Errorf("path = %q", gotPath)
		}
		if gotType != "weather-monitor" {
			t.Errorf("agentType = %q", gotType)
		}
	})

	t.Run("non-201 surfaces an error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "org has no active anchor", http.StatusConflict)
		}))
		defer srv.Close()
		_, err := RegisterRemoteAgent(context.Background(), RemoteRegistration{
			OrchestratorURL: srv.URL, OrgID: "acme", MemberToken: "t", AgentType: "x",
		})
		if err == nil || !strings.Contains(err.Error(), "409") {
			t.Fatalf("want a 409 error, got %v", err)
		}
	})

	t.Run("missing required fields error before any call", func(t *testing.T) {
		_, err := RegisterRemoteAgent(context.Background(), RemoteRegistration{OrgID: "acme"})
		if err == nil {
			t.Fatal("want an error for missing required fields")
		}
	})
}
