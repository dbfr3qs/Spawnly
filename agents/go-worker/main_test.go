// agents/go-worker/main_test.go
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/spawnly/sdk-go"
)

// newSidecarServer mimics the agent-sidecar /token endpoint, handing out token.
func newSidecarServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": token,
			"expires_in":   3600,
		})
	}))
}

// tokenClientFor builds a TokenClient pointed at the test sidecar.
func tokenClientFor(sidecarURL string) *spawnly.TokenClient {
	return spawnly.NewTokenClient(
		spawnly.WithBaseURL(sidecarURL),
		spawnly.WithReadyTimeout(2*time.Second),
		spawnly.WithRetryDelay(10*time.Millisecond),
	)
}

// capturedRequest records what the sample API received.
type capturedRequest struct {
	method string
	path   string
	auth   string
	tenant string
	body   map[string]string
}

// TestRunSuccess exercises the full happy path: the worker fetches a token from
// the sidecar, then POSTs {"task": ...} to /task with the bearer token and
// tenant header, and returns the response's "result" field.
func TestRunSuccess(t *testing.T) {
	sidecar := newSidecarServer(t, "test-token")
	defer sidecar.Close()

	var got capturedRequest
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.tenant = r.Header.Get("X-Tenant-ID")
		_ = json.NewDecoder(r.Body).Decode(&got.body)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "done"})
	}))
	defer api.Close()

	cfg := config{task: "hello", sampleAPIURL: api.URL, tenantID: "tenant-1", scope: "sample-api"}
	result, err := run(context.Background(), cfg, tokenClientFor(sidecar.URL))
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if result != "done" {
		t.Fatalf("result = %q, want %q", result, "done")
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.path != "/task" {
		t.Errorf("path = %q, want /task", got.path)
	}
	if got.auth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want %q", got.auth, "Bearer test-token")
	}
	if got.tenant != "tenant-1" {
		t.Errorf("X-Tenant-ID = %q, want %q", got.tenant, "tenant-1")
	}
	if got.body["task"] != "hello" {
		t.Errorf("body task = %q, want %q", got.body["task"], "hello")
	}
}

// TestRunNoTenant verifies a tenant-less worker omits the X-Tenant-ID header.
func TestRunNoTenant(t *testing.T) {
	sidecar := newSidecarServer(t, "tok")
	defer sidecar.Close()

	var sawTenant bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawTenant = r.Header["X-Tenant-Id"]
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "ok"})
	}))
	defer api.Close()

	cfg := config{task: "t", sampleAPIURL: api.URL, scope: "sample-api"}
	if _, err := run(context.Background(), cfg, tokenClientFor(sidecar.URL)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if sawTenant {
		t.Error("expected no X-Tenant-ID header for tenant-less worker")
	}
}

// TestRunAPIError surfaces a non-200 from the sample API as an error.
func TestRunAPIError(t *testing.T) {
	sidecar := newSidecarServer(t, "tok")
	defer sidecar.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer api.Close()

	cfg := config{task: "t", sampleAPIURL: api.URL, scope: "sample-api"}
	if _, err := run(context.Background(), cfg, tokenClientFor(sidecar.URL)); err == nil {
		t.Fatal("expected error on 500 from sample API")
	}
}

// TestConfigFromEnv covers the env contract: defaults and the required URL.
func TestConfigFromEnv(t *testing.T) {
	t.Setenv("TASK", "do-it")
	t.Setenv("SAMPLE_API_URL", "http://api.example")
	t.Setenv("TENANT_ID", "t-9")
	t.Setenv("SCOPE", "")

	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.scope != "sample-api" {
		t.Errorf("scope default = %q, want sample-api", cfg.scope)
	}
	if cfg.task != "do-it" || cfg.sampleAPIURL != "http://api.example" || cfg.tenantID != "t-9" {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

// TestConfigFromEnvMissingURL asserts SAMPLE_API_URL is required.
func TestConfigFromEnvMissingURL(t *testing.T) {
	t.Setenv("SAMPLE_API_URL", "")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("expected error when SAMPLE_API_URL is unset")
	}
}
