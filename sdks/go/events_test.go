package spawnly

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostEventBodyAndURL(t *testing.T) {
	var gotPath, gotMethod, gotContentType string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	err := PostEvent(context.Background(), srv.URL, "agent-123", "run_start", map[string]any{
		"agentName": "demo",
	})
	if err != nil {
		t.Fatalf("PostEvent: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q", gotMethod)
	}
	if gotPath != "/v1/agents/agent-123/events" {
		t.Fatalf("path = %q", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("content-type = %q", gotContentType)
	}
	if gotBody["source"] != "agent" {
		t.Fatalf("source = %v, want agent", gotBody["source"])
	}
	if gotBody["type"] != "run_start" {
		t.Fatalf("type = %v, want run_start", gotBody["type"])
	}
	payload, ok := gotBody["payload"].(map[string]any)
	if !ok || payload["agentName"] != "demo" {
		t.Fatalf("payload = %v", gotBody["payload"])
	}
}

func TestPostEventReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if err := PostEvent(context.Background(), srv.URL, "a", "t", nil); err == nil {
		t.Fatal("expected error on 5xx response")
	}
}

func TestPostEventReturnsErrorOnUnreachable(t *testing.T) {
	// Best-effort: an unreachable registry returns an error rather than panicking.
	err := PostEvent(context.Background(), "http://127.0.0.1:1", "a", "t", nil)
	if err == nil {
		t.Fatal("expected error against unreachable registry")
	}
}
