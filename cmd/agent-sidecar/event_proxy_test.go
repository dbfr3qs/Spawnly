// cmd/agent-sidecar/event_proxy_test.go
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spawnly/platform/internal/attestor"
)

// TestEventProxyForwardsWithBearer asserts the sidecar's event proxy forwards an
// agent's POST /v1/agents/{id}/events to the registry, attaches the sidecar's SVID
// as a bearer, targets the correct path, and returns the registry's status + body.
func TestEventProxyForwardsWithBearer(t *testing.T) {
	var (
		gotPath   string
		gotAuth   string
		gotBody   string
		gotMethod string
	)
	registry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer registry.Close()

	// Fake bearer: the sidecar shares its svidBearer with the proxy, so drive the
	// handler via a real svidBearer backed by a MockSource returning a known token.
	bearer := &svidBearer{source: &attestor.MockSource{
		Cred: attestor.Credential{Value: "known-svid-token"},
	}}
	handler := newEventProxyHandler(registry.URL, bearer.get)

	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Mirror how run() registers it, so PathValue("id") is populated.
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/agents/{id}/events", handler)
		mux.ServeHTTP(w, r)
	}))
	defer sidecar.Close()

	reqBody := `{"source":"agent","type":"work_ok","payload":{"status":200}}`
	resp, err := http.Post(sidecar.URL+"/v1/agents/worker-1/events", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST to sidecar proxy: %v", err)
	}
	defer resp.Body.Close()
	outBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("proxy returned status %d, want 201 (registry's status propagated)", resp.StatusCode)
	}
	if string(outBody) != `{"ok":true}` {
		t.Errorf("proxy body = %q, want the registry's body", string(outBody))
	}
	if gotMethod != http.MethodPost {
		t.Errorf("registry saw method %q, want POST", gotMethod)
	}
	if gotPath != "/v1/agents/worker-1/events" {
		t.Errorf("registry saw path %q, want /v1/agents/worker-1/events", gotPath)
	}
	if gotAuth != "Bearer known-svid-token" {
		t.Errorf("registry saw Authorization %q, want Bearer known-svid-token", gotAuth)
	}
	if gotBody != reqBody {
		t.Errorf("registry saw body %q, want it forwarded verbatim (%q)", gotBody, reqBody)
	}
}
