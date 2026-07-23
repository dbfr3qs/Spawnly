package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// connectInstall runs a connect against a stub registry and returns the
// response's Install map.
func connectInstall(t *testing.T, api *substrateAPI) map[string]string {
	t.Helper()
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer regSrv.Close()
	api.registryURL = regSrv.URL

	mux := connectMux(api)
	req := httptest.NewRequest("POST", "/v1/orgs/acme/substrate", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("connect status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var resp connectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Install
}

// Hints set (the edge overlay) ⇒ the Install map carries the real endpoints the
// customer substitutes into the bundle (spire-agent server_address/port,
// substrate-agent ORCHESTRATOR_URL).
func TestConnect_InstallEndpointHintsPresentWhenSet(t *testing.T) {
	api := &substrateAPI{
		cpBearer: func() string { return "" }, trustDomain: "cluster.local",
		issuer: newTestIssuer(t), agentNamespace: "substrate-sim",
		agentSA: "substrate-agent", leafTTL: time.Hour,
		spireEndpointHint: "spire.spawnly.run:443",
		publicURLHint:     "https://substrate.spawnly.run",
	}
	install := connectInstall(t, api)
	if got := install["spireServerEndpoint"]; got != "spire.spawnly.run:443" {
		t.Fatalf("spireServerEndpoint = %q, want spire.spawnly.run:443", got)
	}
	if got := install["substrateURL"]; got != "https://substrate.spawnly.run" {
		t.Fatalf("substrateURL = %q, want https://substrate.spawnly.run", got)
	}
	// The baseline keys are untouched by the hints.
	if install["namespace"] != "substrate-sim" || install["serviceAccount"] != "substrate-agent" {
		t.Fatalf("baseline install keys altered: %+v", install)
	}
}

// Hints unset (local kind) ⇒ the keys are ABSENT — not empty-valued — so the
// local connect response stays byte-identical to the pre-edge behavior.
func TestConnect_InstallEndpointHintsAbsentWhenUnset(t *testing.T) {
	api := &substrateAPI{
		cpBearer: func() string { return "" }, trustDomain: "cluster.local",
		issuer: newTestIssuer(t), agentNamespace: "substrate-sim",
		agentSA: "substrate-agent", leafTTL: time.Hour,
	}
	install := connectInstall(t, api)
	if _, ok := install["spireServerEndpoint"]; ok {
		t.Fatal("spireServerEndpoint must be absent when the hint env is unset")
	}
	if _, ok := install["substrateURL"]; ok {
		t.Fatal("substrateURL must be absent when the hint env is unset")
	}
	if len(install) != 3 { // namespace, serviceAccount, note — the pre-edge shape
		t.Fatalf("unexpected install keys: %+v", install)
	}
}

// The env vars feed the API fields (config only — no contract change).
func TestNewSubstrateAPI_EndpointHintEnvWiring(t *testing.T) {
	clearIssuerEnv(t)
	t.Setenv("SPIRE_ENTRY_API_ADDR", "") // no broker dial from an inherited env
	t.Setenv("SUBSTRATE_SPIRE_ENDPOINT_HINT", "spire.spawnly.run:443")
	t.Setenv("SUBSTRATE_PUBLIC_URL_HINT", "https://substrate.spawnly.run")
	api, err := newAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.spireEndpointHint != "spire.spawnly.run:443" || api.publicURLHint != "https://substrate.spawnly.run" {
		t.Fatalf("hint envs not wired: %q / %q", api.spireEndpointHint, api.publicURLHint)
	}

	t.Setenv("SUBSTRATE_SPIRE_ENDPOINT_HINT", "")
	t.Setenv("SUBSTRATE_PUBLIC_URL_HINT", "")
	api, err = newAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.spireEndpointHint != "" || api.publicURLHint != "" {
		t.Fatalf("hints must be empty when env unset: %q / %q", api.spireEndpointHint, api.publicURLHint)
	}
}
