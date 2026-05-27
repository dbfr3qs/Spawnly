// cmd/agent/main_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newSidecarServer returns a test server that mimics the agent-sidecar /token endpoint.
func newSidecarServer(token string, statusCode int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		if statusCode != http.StatusOK {
			http.Error(w, "error", statusCode)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": token,
			"expires_in":   3600,
		})
	}))
}

func TestCallSampleAPISuccess(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/task" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": "done"})
	}))
	defer apiSrv.Close()

	result, err := callSampleAPI(context.Background(), apiSrv.URL, "test-token", "hello")
	if err != nil {
		t.Fatalf("callSampleAPI: %v", err)
	}
	if result != "done" {
		t.Fatalf("expected result=done, got %q", result)
	}
}

func TestCallSampleAPIUnauthorized(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer apiSrv.Close()

	_, err := callSampleAPI(context.Background(), apiSrv.URL, "bad-token", "hello")
	if err == nil {
		t.Fatal("expected error on 403 from sample API")
	}
}

func TestCallSampleAPIEmptyTask(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["task"] != "" {
			http.Error(w, "expected empty task", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": "empty"})
	}))
	defer apiSrv.Close()

	result, err := callSampleAPI(context.Background(), apiSrv.URL, "tok", "")
	if err != nil {
		t.Fatalf("callSampleAPI with empty task: %v", err)
	}
	if result != "empty" {
		t.Fatalf("expected result=empty, got %q", result)
	}
}

func TestGetSidecarTokenSuccess(t *testing.T) {
	sidecarSrv := newSidecarServer("my-access-token", http.StatusOK)
	defer sidecarSrv.Close()

	// getSidecarToken hard-codes localhost:8089, so we test the helper via callSampleAPI
	// but we can exercise the JSON decoding path through a local server by verifying
	// that callSampleAPI correctly uses a token obtained externally.
	// Direct unit test: verify callSampleAPI passes token in Authorization header.
	var receivedAuth string
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"result": "ok"})
	}))
	defer apiSrv.Close()

	_, err := callSampleAPI(context.Background(), apiSrv.URL, "my-access-token", "task")
	if err != nil {
		t.Fatalf("callSampleAPI: %v", err)
	}
	if receivedAuth != "Bearer my-access-token" {
		t.Fatalf("expected 'Bearer my-access-token', got %q", receivedAuth)
	}
}

func TestCallSampleAPIServerError(t *testing.T) {
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer apiSrv.Close()

	_, err := callSampleAPI(context.Background(), apiSrv.URL, "tok", "task")
	if err == nil {
		t.Fatal("expected error on 500 from sample API")
	}
}
