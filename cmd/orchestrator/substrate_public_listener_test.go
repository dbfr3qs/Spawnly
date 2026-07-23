package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// TestSubstratePublicMux_ServesOnlySubstrateSurface asserts the dedicated public
// listener exposes EXACTLY the substrate surface and nothing else: every substrate
// route is present (does not 404), and every non-substrate orchestrator route
// (/spawn, /v1/agents, /health, the admin family) is absent (404s). This is the
// edge-minimality invariant — the ALB targets this mux, so a non-substrate route
// being reachable here would widen the internet-facing surface.
func TestSubstratePublicMux_ServesOnlySubstrateSurface(t *testing.T) {
	clearBrokerEnv(t)
	// A bare substrateAPI: no CA, no broker — the routes are still registered, which
	// is all this test cares about (presence vs 404, not handler success).
	api, err := newSubstrateAPI(registry.NewMock(nil), "http://registry.invalid", func() string { return "" }, newSubstrateHub())
	if err != nil {
		t.Fatalf("build substrate api: %v", err)
	}

	mux := substratePublicMux(api, fakeValidator{claims: humanClaims("alice", "orchestrator:read", "orchestrator:write")},
		"orchestrator", "orchestrator:read", "orchestrator:write", spicedb.NewMock())

	// Substrate routes: MUST be served (present ⇒ not a 404-from-missing-route).
	substrateRoutes := []struct {
		method, path string
	}{
		{"GET", "/v1/substrate/agents"},
		{"POST", "/v1/substrate/entries"},
		{"DELETE", "/v1/substrate/entries?spiffeId=spiffe://cluster.local/org/acme/agent/t/u/ty/x"},
		{"GET", "/v1/orgs/acme/substrate"},
	}
	for _, rt := range substrateRoutes {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code == http.StatusNotFound {
			t.Errorf("substrate route %s %s returned 404 — it must be served on the public mux", rt.method, rt.path)
		}
	}

	// Non-substrate routes: MUST be absent (404). These are the routes the edge must
	// never expose.
	forbidden := []struct {
		method, path string
	}{
		{"POST", "/spawn"},
		{"GET", "/v1/agents"},
		{"GET", "/health"},
		{"GET", "/v1/admin/orgs"},
		{"GET", "/v1/templates"},
		{"GET", "/v1/my/orgs"},
	}
	for _, rt := range forbidden {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("non-substrate route %s %s returned %d — it must NOT be reachable on the public substrate listener (want 404)", rt.method, rt.path, rec.Code)
		}
	}
}
