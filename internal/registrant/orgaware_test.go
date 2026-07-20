package registrant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/registry"
)

// --- test doubles -----------------------------------------------------------

// stubStore implements orgStore from in-memory maps, with an optional forced
// error on GetOrg to exercise the fail-closed path.
type stubStore struct {
	agents    map[string]registry.AgentRecord
	orgs      map[string]registry.Organization
	orgErr    error
	getOrgs   int
	getAgents int
}

func (s *stubStore) GetAgent(_ context.Context, id string) (registry.AgentRecord, error) {
	s.getAgents++
	return s.agents[id], nil
}

func (s *stubStore) GetOrg(_ context.Context, id string) (registry.Organization, bool, error) {
	s.getOrgs++
	if s.orgErr != nil {
		return registry.Organization{}, false, s.orgErr
	}
	o, ok := s.orgs[id]
	return o, ok, nil
}

// recordingDefault is a stub default Verifier that records whether it was called
// and returns a canned identity (or error).
type recordingDefault struct {
	called   bool
	identity Identity
	err      error
}

func (d *recordingDefault) Verify(_ context.Context, _ *http.Request) (Identity, error) {
	d.called = true
	return d.identity, d.err
}

// fakeSTS answers GetCallerIdentity with the principal named by the ?principal=
// query param, so one server can attest many identities. It performs no SigV4
// validation — a trust-injection stub for tests only.
type principal struct{ account, arn string }

func fakeSTS(t *testing.T, principals map[string]principal) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Action") != "GetCallerIdentity" {
			http.Error(w, "bad action", http.StatusBadRequest)
			return
		}
		p, ok := principals[r.URL.Query().Get("principal")]
		if !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/xml")
		fmt.Fprintf(w, `<GetCallerIdentityResponse><GetCallerIdentityResult>`+
			`<Arn>%s</Arn><Account>%s</Account><UserId>AID</UserId>`+
			`</GetCallerIdentityResult></GetCallerIdentityResponse>`, p.arn, p.account)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// stsCred builds a base64 presigned envelope targeting srv for the given
// principal key, exactly the shape internal/attestor decodes.
func stsCred(t *testing.T, srv *httptest.Server, principalKey string) string {
	t.Helper()
	env := map[string]any{
		"method": "POST",
		"url":    srv.URL + "/?Action=GetCallerIdentity&principal=" + principalKey,
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func stsRequest(cred string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+cred)
	return req
}

const (
	acmeAccount   = "111122223333"
	globexAccount = "999988887777"
	acmeRole      = "arn:aws:sts::111122223333:assumed-role/acme-agents/*"
	globexRole    = "arn:aws:sts::999988887777:assumed-role/globex-agents/*"
)

func acmeArn(session string) string {
	return "arn:aws:sts::111122223333:assumed-role/acme-agents/" + session
}

func activeStsAnchor(account, rolePattern string) *registry.AttestationAnchor {
	return &registry.AttestationAnchor{Type: "sts", AWSAccountID: account, RoleArnPattern: rolePattern, Status: registry.AnchorActive}
}

// --- the isolation matrix ---------------------------------------------------

// org-A credential × org-A record ⇒ verified. The AWS-attested account matches
// the record's org anchor account and the ARN matches the role pattern.
func TestOrgAware_StsCredMatchesOwnOrg_Verified(t *testing.T) {
	t.Setenv("STS_TRUSTED_ENDPOINT", "") // set below to the fake server
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "acme"}},
		orgs:   map[string]registry.Organization{"acme": {OrgID: "acme", Anchor: activeStsAnchor(acmeAccount, acmeRole)}},
	}
	def := &recordingDefault{}
	v := NewOrgAwareVerifier(def, store, sts.Client())

	id, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme")))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.AgentID != "remote-1" || id.Issuer != "aws-sts" {
		t.Fatalf("identity = %+v", id)
	}
	if def.called {
		t.Fatal("default verifier must NOT be consulted on the STS org path")
	}
}

// org-A credential × org-B record ⇒ refused: the attested account (acme) is not
// org B's (globex) anchor account. The credential can never authenticate an
// agent recorded under another org.
func TestOrgAware_StsCredCrossOrg_Refused(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "globex"}},
		orgs:   map[string]registry.Organization{"globex": {OrgID: "globex", Anchor: activeStsAnchor(globexAccount, globexRole)}},
	}
	v := NewOrgAwareVerifier(&recordingDefault{}, store, sts.Client())

	if _, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme"))); err == nil {
		t.Fatal("expected refusal for a cross-org STS credential")
	}
}

// unanchored-org record × any STS credential ⇒ refused (no anchor to ride, no
// fallback to the default verifier).
func TestOrgAware_UnanchoredOrg_Refused(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "acme"}},
		orgs:   map[string]registry.Organization{"acme": {OrgID: "acme"}}, // Anchor nil
	}
	def := &recordingDefault{}
	v := NewOrgAwareVerifier(def, store, sts.Client())

	if _, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme"))); err == nil {
		t.Fatal("expected refusal for an unanchored org")
	}
	if def.called {
		t.Fatal("must NOT fall back to the default verifier for an anchored-agent STS cred")
	}
}

// anchor pending ⇒ treated as unanchored ⇒ refused.
func TestOrgAware_PendingAnchor_Refused(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	pending := activeStsAnchor(acmeAccount, acmeRole)
	pending.Status = registry.AnchorPending
	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "acme"}},
		orgs:   map[string]registry.Organization{"acme": {OrgID: "acme", Anchor: pending}},
	}
	v := NewOrgAwareVerifier(&recordingDefault{}, store, sts.Client())

	if _, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme"))); err == nil {
		t.Fatal("expected refusal for a pending anchor")
	}
}

// role-ARN pattern mismatch ⇒ refused even when the account matches.
func TestOrgAware_RolePatternMismatch_Refused(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "acme"}},
		orgs: map[string]registry.Organization{"acme": {OrgID: "acme",
			Anchor: activeStsAnchor(acmeAccount, "arn:aws:sts::111122223333:assumed-role/other-role/*")}},
	}
	v := NewOrgAwareVerifier(&recordingDefault{}, store, sts.Client())

	if _, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme"))); err == nil {
		t.Fatal("expected refusal for a role-pattern mismatch")
	}
}

// unknown record (the STS credential attests an agentId with no registry
// record) ⇒ refused.
func TestOrgAware_UnknownRecord_Refused(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("ghost")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{agents: map[string]registry.AgentRecord{}, orgs: map[string]registry.Organization{}}
	v := NewOrgAwareVerifier(&recordingDefault{}, store, sts.Client())

	if _, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme"))); err == nil {
		t.Fatal("expected refusal for an unknown agent record")
	}
}

// getOrg outage ⇒ fail CLOSED (error propagates; never mint against an unknown
// anchor).
func TestOrgAware_GetOrgOutage_FailsClosed(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "acme"}},
		orgErr: errors.New("dynamodb throttled"),
	}
	v := NewOrgAwareVerifier(&recordingDefault{}, store, sts.Client())

	if _, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme"))); err == nil {
		t.Fatal("expected getOrg outage to fail closed")
	}
}

// A non-STS credential (a JWT-SVID/OIDC/mTLS request) is delegated verbatim to
// the default verifier — the platform SPIRE path is untouched by anchors.
func TestOrgAware_NonStsCredential_DelegatesToDefault(t *testing.T) {
	def := &recordingDefault{identity: Identity{AgentID: "agent-abc", Subject: "spiffe://x/agent/agent-abc", Issuer: "spiffe-svid"}}
	store := &stubStore{}
	v := NewOrgAwareVerifier(def, store, nil)

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer header.payload.signature") // JWT shape
	id, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !def.called {
		t.Fatal("default verifier must handle a non-STS credential")
	}
	if id.AgentID != "agent-abc" {
		t.Fatalf("identity = %+v", id)
	}
	if store.getOrgs != 0 {
		t.Fatal("SPIRE path must not touch the org store")
	}
}

// An org-less agent presenting an STS credential is decided by the default
// verifier (preserves the pluggable-attestation aws-sts deployment).
func TestOrgAware_OrgLessStsAgent_DelegatesToDefault(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: ""}},
	}
	def := &recordingDefault{identity: Identity{AgentID: "remote-1", Subject: acmeArn("remote-1"), Issuer: "aws-sts"}}
	v := NewOrgAwareVerifier(def, store, sts.Client())

	id, err := v.Verify(context.Background(), stsRequest(stsCred(t, sts, "acme")))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !def.called {
		t.Fatal("org-less STS agent must be decided by the default verifier")
	}
	if id.AgentID != "remote-1" {
		t.Fatalf("identity = %+v", id)
	}
}
