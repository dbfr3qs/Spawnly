package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate"
	"github.com/spawnly/platform/internal/substrate/devca"
)

// fakeBroker records created/deleted entries for the Entry API tests.
type fakeBroker struct {
	created []substrate.EntrySpec
	deleted []string
}

func (f *fakeBroker) CreateEntry(_ context.Context, s substrate.EntrySpec) error {
	f.created = append(f.created, s)
	return nil
}
func (f *fakeBroker) DeleteEntry(_ context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// fakeSVIDAuth stands in for the JWKS-backed substrate-agent authenticator.
type fakeSVIDAuth struct {
	org string
	err error
}

func (f fakeSVIDAuth) authenticate(_ context.Context, _ *http.Request) (string, error) {
	return f.org, f.err
}

func newTestIssuer(t *testing.T) *substrate.Issuer {
	t.Helper()
	ca, err := devca.GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("gen ca: %v", err)
	}
	iss, err := substrate.NewIssuer(ca, "cluster.local", time.Hour)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	return iss
}

// connectMux wires only the connect handler (no auth wrapper) so PathValue{id}
// resolves; auth gating is exercised by requireOrgAdmin's own tests.
func connectMux(api *substrateAPI) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orgs/{id}/substrate", api.connect)
	return mux
}

func TestConnect_IssuesOrgBoundIdentity(t *testing.T) {
	// Fake registry that accepts the substrate PUT.
	var gotPut string
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			gotPut = r.URL.Path
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer regSrv.Close()

	broker := &fakeBroker{}
	api := &substrateAPI{
		registryURL:    regSrv.URL,
		cpBearer:       func() string { return "" },
		trustDomain:    "cluster.local",
		issuer:         newTestIssuer(t),
		entries:        &substrate.EntryService{Broker: broker, Resolver: registryRecordOrgResolver{reg: registry.NewMock(nil)}, TrustDomain: "cluster.local"},
		agentNamespace: "substrate-sim",
		agentSA:        "substrate-agent",
		leafTTL:        time.Hour,
	}
	mux := connectMux(api)

	req := httptest.NewRequest("POST", "/v1/orgs/acme/substrate", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("connect status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("one-time credential must be no-store, got %q", rec.Header().Get("Cache-Control"))
	}
	var resp connectResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.OrgID != "acme" || resp.Status != registry.SubstratePending {
		t.Fatalf("resp = %+v", resp)
	}
	// The issued leaf must be bound to the org (CommonName).
	org, err := api.issuer.OrgID([]byte(resp.Identity.CertPEM))
	if err != nil || org != "acme" {
		t.Fatalf("issued leaf org = %q, err=%v; want acme", org, err)
	}
	if resp.Identity.KeyPEM == "" {
		t.Fatal("identity key must be returned once")
	}
	if gotPut != "/v1/orgs/acme/substrate" {
		t.Fatalf("registry PUT path = %q", gotPut)
	}
	// The substrate-agent bootstrap entry must have been provisioned.
	if len(broker.created) != 1 || !strings.HasSuffix(broker.created[0].SPIFFEID, "/org/acme/substrate-agent") {
		t.Fatalf("bootstrap entry not provisioned: %+v", broker.created)
	}
}

// A mixed-case org path normalizes to the lowercase slug in the issued identity.
func TestConnect_NormalizesOrgCase(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer regSrv.Close()

	api := &substrateAPI{
		registryURL: regSrv.URL, cpBearer: func() string { return "" },
		trustDomain: "cluster.local", issuer: newTestIssuer(t),
		agentNamespace: "substrate-sim", agentSA: "substrate-agent", leafTTL: time.Hour,
	}
	mux := connectMux(api)
	req := httptest.NewRequest("POST", "/v1/orgs/Acme/substrate", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body.String())
	}
	var resp connectResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.OrgID != "acme" {
		t.Fatalf("org not normalized: %q", resp.OrgID)
	}
}

func TestConnect_Unconfigured503(t *testing.T) {
	api := &substrateAPI{trustDomain: "cluster.local"} // no issuer
	mux := connectMux(api)
	req := httptest.NewRequest("POST", "/v1/orgs/acme/substrate", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// The Entry API authenticates the caller's substrate SVID and authorizes the
// request under that org — the handler-level slice of the matrix.
func TestEntryAPI_CreateAuthorizedAndCrossOrg(t *testing.T) {
	reg := registry.NewMock(nil)
	reg.Agents = []registry.AgentRecord{{AgentID: "agent-a", OrgID: "acme"}, {AgentID: "agent-b", OrgID: "globex"}}
	// The Entry API gates on the caller org having a CONNECTED substrate record:
	// serve GET /v1/orgs/acme with a substrate present.
	orgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"orgId":     "acme",
			"substrate": map[string]string{"status": "pending", "certFingerprint": "fp"},
		})
	}))
	defer orgSrv.Close()
	broker := &fakeBroker{}
	api := &substrateAPI{
		trustDomain: "cluster.local",
		registryURL: orgSrv.URL,
		cpBearer:    func() string { return "" },
		entries:     &substrate.EntryService{Broker: broker, Resolver: registryRecordOrgResolver{reg: reg}, TrustDomain: "cluster.local"},
		svidAuth:    fakeSVIDAuth{org: "acme"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/substrate/entries", api.createEntry)

	body := func(spiffeID, parent string) string {
		b, _ := json.Marshal(map[string]any{
			"spiffeId":  spiffeID,
			"parentId":  parent,
			"selectors": []string{"k8s:ns:substrate-sim", "k8s:pod-name:p", "k8s:sa:worker"},
		})
		return string(b)
	}
	nodePrefix, _ := devca.NodeSpiffePathPrefix("cluster.local", "acme")

	// Authorized: own org.
	req := httptest.NewRequest("POST", "/v1/substrate/entries",
		strings.NewReader(body("spiffe://cluster.local/org/acme/agent/t/u/ty/agent-a", nodePrefix+"fp")))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("authorized create status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(broker.created) != 1 {
		t.Fatalf("broker created %d, want 1", len(broker.created))
	}

	// Cross-org: caller acme requests an entry whose id names org-B's record ⇒ 403.
	req2 := httptest.NewRequest("POST", "/v1/substrate/entries",
		strings.NewReader(body("spiffe://cluster.local/org/acme/agent/t/u/ty/agent-b", nodePrefix+"fp")))
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("cross-org create status = %d, want 403", rec2.Code)
	}
	if len(broker.created) != 1 {
		t.Fatal("cross-org request must not create an entry")
	}
}

// An unknown agentId is an oracle-free 403 (refuse), NOT a 502 — the resolver
// must distinguish ErrAgentNotFound from a registry outage.
func TestEntryAPI_UnknownAgentIs403(t *testing.T) {
	reg := registry.NewMock(nil) // no agents ⇒ GetAgent returns ErrAgentNotFound
	orgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"orgId": "acme", "substrate": map[string]string{"status": "pending"}})
	}))
	defer orgSrv.Close()
	broker := &fakeBroker{}
	api := &substrateAPI{
		trustDomain: "cluster.local", registryURL: orgSrv.URL, cpBearer: func() string { return "" },
		entries:  &substrate.EntryService{Broker: broker, Resolver: registryRecordOrgResolver{reg: reg}, TrustDomain: "cluster.local"},
		svidAuth: fakeSVIDAuth{org: "acme"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/substrate/entries", api.createEntry)
	nodePrefix, _ := devca.NodeSpiffePathPrefix("cluster.local", "acme")
	b, _ := json.Marshal(map[string]any{
		"spiffeId":  "spiffe://cluster.local/org/acme/agent/t/u/ty/ghost",
		"parentId":  nodePrefix + "fp",
		"selectors": []string{"k8s:ns:substrate-sim", "k8s:pod-name:p", "k8s:sa:worker"},
	})
	req := httptest.NewRequest("POST", "/v1/substrate/entries", strings.NewReader(string(b)))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unknown agent status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if len(broker.created) != 0 {
		t.Fatal("unknown agent must not create an entry")
	}
}

// A DISCONNECTED org (no substrate record) is refused even with a valid SVID —
// the substrate record is the authoritative gate.
func TestEntryAPI_NoSubstrateRecordRefused(t *testing.T) {
	orgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"orgId": "acme"}) // no substrate
	}))
	defer orgSrv.Close()
	broker := &fakeBroker{}
	api := &substrateAPI{
		trustDomain: "cluster.local", registryURL: orgSrv.URL, cpBearer: func() string { return "" },
		entries:  &substrate.EntryService{Broker: broker, Resolver: registryRecordOrgResolver{reg: registry.NewMock(nil)}, TrustDomain: "cluster.local"},
		svidAuth: fakeSVIDAuth{org: "acme"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/substrate/entries", api.createEntry)
	req := httptest.NewRequest("POST", "/v1/substrate/entries", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-substrate status = %d, want 403", rec.Code)
	}
}

func TestEntryAPI_Unauthenticated401(t *testing.T) {
	api := &substrateAPI{
		trustDomain: "cluster.local",
		entries:     &substrate.EntryService{Broker: &fakeBroker{}, Resolver: registryRecordOrgResolver{reg: registry.NewMock(nil)}, TrustDomain: "cluster.local"},
		svidAuth:    fakeSVIDAuth{err: http.ErrNoCookie},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/substrate/entries", api.createEntry)
	req := httptest.NewRequest("POST", "/v1/substrate/entries", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestEntryAPI_Unconfigured503(t *testing.T) {
	api := &substrateAPI{trustDomain: "cluster.local"} // no entries/svidAuth
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/substrate/entries", api.createEntry)
	req := httptest.NewRequest("POST", "/v1/substrate/entries", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
