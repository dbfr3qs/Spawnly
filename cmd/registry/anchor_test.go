package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spicedb"
)

// seedOrg puts a bare org record into the store for anchor tests.
func seedOrg(t *testing.T, s registry.Store, id string) {
	t.Helper()
	if err := s.PutOrg(context.Background(), registry.Organization{OrgID: id, Name: id}); err != nil {
		t.Fatalf("seed org %s: %v", id, err)
	}
}

// stubSTS is a fake STS endpoint for activate tests: it answers
// GetCallerIdentity with a fixed account + ARN. Its URL is registered as the
// trusted endpoint so the verifier's SSRF guard admits it.
func stubSTS(t *testing.T, account, arn string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<GetCallerIdentityResponse><GetCallerIdentityResult>` +
			`<Arn>` + arn + `</Arn><Account>` + account + `</Account>` +
			`</GetCallerIdentityResult></GetCallerIdentityResponse>`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("STS_TRUSTED_ENDPOINT", srv.URL)
	return srv
}

// credentialFor builds a base64 presigned-request credential pointing at the
// (trusted) stub STS. The fake ignores the signature, so no real SigV4 is
// needed — this stands in for what cmd/anchorctl presigns from real AWS creds.
func credentialFor(t *testing.T, stsURL string) string {
	t.Helper()
	pr := map[string]any{"method": "GET", "url": stsURL + "/?Action=GetCallerIdentity"}
	b, _ := json.Marshal(pr)
	return base64.StdEncoding.EncodeToString(b)
}

func TestAnchor_RegisterLandsPending(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())

	rec := doReq(t, mux, "PUT", "/v1/orgs/acme/anchor", map[string]string{
		"type": "sts", "awsAccountId": "111122223333",
		"roleArnPattern": "arn:aws:sts::111122223333:assumed-role/acme-agents/*",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	org, _, _ := s.GetOrg(context.Background(), "acme")
	if org.Anchor == nil || org.Anchor.Status != registry.AnchorPending {
		t.Fatalf("anchor = %+v, want pending", org.Anchor)
	}
	if org.Anchor.AWSAccountID != "111122223333" {
		t.Fatalf("anchor account = %q", org.Anchor.AWSAccountID)
	}
}

func TestAnchor_RegisterValidatesBody(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	cases := []map[string]string{
		{"type": "oidc", "awsAccountId": "111122223333", "roleArnPattern": "arn:x"}, // bad type
		{"type": "sts", "awsAccountId": "12345", "roleArnPattern": "arn:x"},         // bad account
		{"type": "sts", "awsAccountId": "111122223333", "roleArnPattern": "*"},      // catch-all pattern
	}
	for _, body := range cases {
		if rec := doReq(t, mux, "PUT", "/v1/orgs/acme/anchor", body); rec.Code != http.StatusBadRequest {
			t.Errorf("register %v status = %d, want 400", body, rec.Code)
		}
	}
	// Unknown org => 404.
	if rec := doReq(t, mux, "PUT", "/v1/orgs/ghost/anchor", map[string]string{
		"type": "sts", "awsAccountId": "111122223333", "roleArnPattern": "arn:x",
	}); rec.Code != http.StatusNotFound {
		t.Errorf("register unknown org status = %d, want 404", rec.Code)
	}
}

func TestAnchor_Delete(t *testing.T) {
	s := newStore()
	seedOrg(t, s, "acme")
	mux := orgMux(s, spicedb.NewMock(), newFakeDirectory())
	doReq(t, mux, "PUT", "/v1/orgs/acme/anchor", map[string]string{
		"type": "sts", "awsAccountId": "111122223333", "roleArnPattern": "arn:aws:sts::111122223333:assumed-role/r/*",
	})
	if rec := doReq(t, mux, "DELETE", "/v1/orgs/acme/anchor", nil); rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
	org, _, _ := s.GetOrg(context.Background(), "acme")
	if org.Anchor != nil {
		t.Fatalf("anchor = %+v, want nil after delete", org.Anchor)
	}
}

func TestActivateAnchor_RightAccountAndRole(t *testing.T) {
	const account = "111122223333"
	arn := "arn:aws:sts::" + account + ":assumed-role/acme-agents/agent-1"
	sts := stubSTS(t, account, arn)

	s := newStore()
	s.PutOrg(context.Background(), registry.Organization{OrgID: "acme", Anchor: &registry.AttestationAnchor{
		Type: "sts", AWSAccountID: account,
		RoleArnPattern: "arn:aws:sts::" + account + ":assumed-role/acme-agents/*",
		Status:         registry.AnchorPending,
	}})

	org, status, err := activateAnchor(context.Background(), s, sts.Client(), "acme", credentialFor(t, sts.URL))
	if err != nil || status != http.StatusOK {
		t.Fatalf("activate = %d, %v; want 200, nil", status, err)
	}
	if org.Anchor.Status != registry.AnchorActive || org.Anchor.ActivatedAt == nil {
		t.Fatalf("anchor = %+v, want active+activatedAt", org.Anchor)
	}
	// Persisted.
	got, _, _ := s.GetOrg(context.Background(), "acme")
	if got.Anchor.Status != registry.AnchorActive {
		t.Fatalf("persisted anchor status = %q, want active", got.Anchor.Status)
	}
}

func TestActivateAnchor_WrongAccountStaysPending(t *testing.T) {
	// The stub attests a DIFFERENT account than the anchor expects.
	sts := stubSTS(t, "999988887777", "arn:aws:sts::999988887777:assumed-role/acme-agents/agent-1")
	s := newStore()
	s.PutOrg(context.Background(), registry.Organization{OrgID: "acme", Anchor: &registry.AttestationAnchor{
		Type: "sts", AWSAccountID: "111122223333",
		RoleArnPattern: "arn:aws:sts::111122223333:assumed-role/acme-agents/*",
		Status:         registry.AnchorPending,
	}})

	_, status, err := activateAnchor(context.Background(), s, sts.Client(), "acme", credentialFor(t, sts.URL))
	if err == nil || status != http.StatusBadRequest {
		t.Fatalf("activate wrong account = %d, %v; want 400, err", status, err)
	}
	got, _, _ := s.GetOrg(context.Background(), "acme")
	if got.Anchor.Status != registry.AnchorPending {
		t.Fatalf("anchor status = %q, want still pending", got.Anchor.Status)
	}
}

func TestActivateAnchor_WrongRoleStaysPending(t *testing.T) {
	const account = "111122223333"
	// Right account, but a role outside the anchor's pattern.
	sts := stubSTS(t, account, "arn:aws:sts::"+account+":assumed-role/other-role/agent-1")
	s := newStore()
	s.PutOrg(context.Background(), registry.Organization{OrgID: "acme", Anchor: &registry.AttestationAnchor{
		Type: "sts", AWSAccountID: account,
		RoleArnPattern: "arn:aws:sts::" + account + ":assumed-role/acme-agents/*",
		Status:         registry.AnchorPending,
	}})
	_, status, err := activateAnchor(context.Background(), s, sts.Client(), "acme", credentialFor(t, sts.URL))
	if err == nil || status != http.StatusBadRequest {
		t.Fatalf("activate wrong role = %d, %v; want 400, err", status, err)
	}
}

func TestActivateAnchor_NoAnchorOrEmptyCredential(t *testing.T) {
	sts := stubSTS(t, "111122223333", "arn:aws:sts::111122223333:assumed-role/r/a")
	s := newStore()
	seedOrg(t, s, "acme") // org with NO anchor
	if _, status, err := activateAnchor(context.Background(), s, sts.Client(), "acme", credentialFor(t, sts.URL)); err == nil || status != http.StatusBadRequest {
		t.Fatalf("activate no-anchor = %d, %v; want 400, err", status, err)
	}
	// Empty credential.
	s.PutOrg(context.Background(), registry.Organization{OrgID: "acme", Anchor: &registry.AttestationAnchor{
		Type: "sts", AWSAccountID: "111122223333", RoleArnPattern: "arn:x", Status: registry.AnchorPending,
	}})
	if _, status, err := activateAnchor(context.Background(), s, sts.Client(), "acme", ""); err == nil || status != http.StatusBadRequest {
		t.Fatalf("activate empty credential = %d, %v; want 400, err", status, err)
	}
}

// TestActivateRoute_OracleFree proves the HTTP route returns a generic 400 body
// on a proof failure — no detail that would distinguish the failure reason.
func TestActivateRoute_OracleFree(t *testing.T) {
	sts := stubSTS(t, "999988887777", "arn:aws:sts::999988887777:assumed-role/r/a")
	s := newStore()
	s.PutOrg(context.Background(), registry.Organization{OrgID: "acme", Anchor: &registry.AttestationAnchor{
		Type: "sts", AWSAccountID: "111122223333", RoleArnPattern: "arn:aws:sts::111122223333:assumed-role/r/*", Status: registry.AnchorPending,
	}})
	// Route wired with a client that trusts the stub.
	mux := http.NewServeMux()
	registerAnchorRoutes(mux, s, func(h http.HandlerFunc) http.HandlerFunc { return h }, sts.Client())
	rec := doReq(t, mux, "POST", "/v1/orgs/acme/anchor/activate", map[string]string{"credential": credentialFor(t, sts.URL)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "activation failed") {
		t.Fatalf("body = %q, want generic 'activation failed'", body)
	}
	for _, leak := range []string{"account", "mismatch", "pending", "anchor"} {
		if strings.Contains(body, leak) {
			t.Fatalf("body leaks detail %q: %q", leak, body)
		}
	}
}
