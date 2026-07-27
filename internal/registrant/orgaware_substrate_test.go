package registrant

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spiffe"
)

// Substrate-edge COEXISTENCE tests (spec AC-5a). These pin the ONE deployment
// where the default registrant verifier is aws-stsweb (platform EKS agents) yet
// a substrate workload must self-register with a SPIFFE JWT-SVID. WithSpiffe
// installs a SEPARATE SPIFFE verifier for the non-STS branch; the STS path and
// the org-less STS fallback to `def` stay exactly as they were.

// A NON-STS credential is routed to the substrate SPIFFE verifier, NOT to the
// aws-stsweb default, and the org binding is still applied via bindSpireOrg.
func TestSubstrate_NonStsCred_RoutedToSpiffeNotDef(t *testing.T) {
	def := &recordingDefault{} // the aws-stsweb default — must NOT see the SVID
	spf := &recordingDefault{identity: Identity{AgentID: "agent-a", OrgID: "acme", Issuer: "spiffe-svid"}}
	store := twoOrgStore()

	v := NewOrgAwareVerifier(def, store, nil).WithSpiffe(spf)

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !spf.called {
		t.Fatal("the substrate SPIFFE verifier must handle the non-STS credential")
	}
	if def.called {
		t.Fatal("the aws-stsweb default must NOT be consulted for a non-STS credential when a spiffe branch is wired")
	}
	if id.AgentID != "agent-a" || id.OrgID != "acme" {
		t.Fatalf("identity = %+v", id)
	}
	if store.getAgents == 0 {
		t.Fatal("org binding (bindSpireOrg) must consult the store for an org-scoped SVID")
	}
}

// End-to-end coexistence with a REAL SpiffeVerifier as the substrate branch:
// this is the live fix. Default = aws-stsweb stand-in (never reached), substrate
// branch = trust-domain-enforcing SpiffeVerifier. An org-A SVID × org-A record
// verifies (the self-register 401 is gone) and the aws-stsweb default is never
// consulted.
func TestSubstrate_RealSpiffeBranch_OrgAMatches_Verified(t *testing.T) {
	def := &recordingDefault{} // aws-stsweb default — must NOT be reached
	sv := NewSpiffeVerifier(&spiffe.MockSVIDValidator{
		SpiffeID: "spiffe://cluster.local/org/acme/agent/t/alice/worker/agent-a",
	})
	sv.TrustDomain = spireTD

	v := NewOrgAwareVerifier(def, twoOrgStore(), nil).WithSpiffe(sv)

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if def.called {
		t.Fatal("the aws-stsweb default must never validate a substrate SVID")
	}
	if id.AgentID != "agent-a" || id.OrgID != "acme" || id.Issuer != "spiffe-svid" {
		t.Fatalf("identity = %+v", id)
	}
}

// ISOLATION INVARIANT through the spiffe branch: an org-A SVID naming an org-B
// record is refused (bindSpireOrg), and the aws-stsweb default is never a
// fallback. Mirrors TestSpireOrg_CrossOrgRecord_Refused but with the SPIFFE
// verifier installed as the substrate branch over an stsweb default.
func TestSubstrate_RealSpiffeBranch_CrossOrgRecord_Refused(t *testing.T) {
	def := &recordingDefault{}
	// SVID claims org acme but names agent-b, an org-B (globex) record.
	sv := NewSpiffeVerifier(&spiffe.MockSVIDValidator{
		SpiffeID: "spiffe://cluster.local/org/acme/agent/t/alice/worker/agent-b",
	})
	sv.TrustDomain = spireTD

	v := NewOrgAwareVerifier(def, twoOrgStore(), nil).WithSpiffe(sv)

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected refusal for a cross-org SVID/record pairing through the spiffe branch")
	}
	if def.called {
		t.Fatal("must NOT fall back to the aws-stsweb default when the spiffe branch refuses")
	}
}

// An STS-shaped credential is UNAFFECTED by the spiffe branch: it still takes
// the org-bound STS path (attested account + anchor), and NEITHER def NOR the
// spiffe branch is consulted for the verification decision.
func TestSubstrate_StsCred_StillTakesStsPath(t *testing.T) {
	sts := fakeSTS(t, map[string]principal{"acme": {acmeAccount, acmeArn("remote-1")}})
	t.Setenv("STS_TRUSTED_ENDPOINT", sts.URL)

	store := &stubStore{
		agents: map[string]registry.AgentRecord{"remote-1": {AgentID: "remote-1", OrgID: "acme"}},
		orgs:   map[string]registry.Organization{"acme": {OrgID: "acme", Anchor: activeStsAnchor(acmeAccount, acmeRole)}},
	}
	def := &recordingDefault{}
	spf := &recordingDefault{}
	v := NewOrgAwareVerifier(def, store, sts.Client()).WithSpiffe(spf)

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
	if spf.called {
		t.Fatal("the spiffe branch must NEVER see an STS-shaped credential")
	}
}

// REGRESSION GUARD (the coexistence trap): an stsweb web-identity JWT is non-STS-
// *shaped* (looksLikeStsCredential matches only the presigned aws-sts envelope),
// so it shares the non-STS branch with SPIFFE SVIDs. With a spiffe branch wired,
// it MUST still route to the aws-stsweb `def` — its sub is NOT a spiffe:// ID — or
// platform agent self-registration breaks (the exact AC-5a coexistence this fix
// must not trade away). Routes to def, and the spiffe branch never sees it.
func TestSubstrate_StswebJwt_RoutesToDefNotSpiffe(t *testing.T) {
	def := &recordingDefault{identity: Identity{AgentID: "plat-1", Issuer: "aws-stsweb"}}
	spf := &recordingDefault{} // the spiffe branch — must NOT be consulted
	store := twoOrgStore()

	v := NewOrgAwareVerifier(def, store, nil).WithSpiffe(spf)

	// A JWT whose sub is the EKS/stsweb subject shape, NOT a spiffe:// ID.
	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithSub("system:serviceaccount:default:spawnly-agent"))

	id, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !def.called {
		t.Fatal("an stsweb JWT (non-spiffe sub) must be validated by the aws-stsweb default")
	}
	if spf.called {
		t.Fatal("the spiffe branch must NOT see an stsweb JWT — that would break platform self-registration")
	}
	if id.AgentID != "plat-1" {
		t.Fatalf("identity = %+v", id)
	}
}

// BACK-COMPAT: with NO spiffe branch (nil), a non-STS credential still goes to
// `def` exactly as before — an unset toggle changes nothing.
func TestSubstrate_NilSpiffeBranch_NonStsGoesToDef(t *testing.T) {
	def := &recordingDefault{identity: Identity{AgentID: "agent-a", OrgID: "acme", Issuer: "spiffe-svid"}}
	store := twoOrgStore()
	v := NewOrgAwareVerifier(def, store, nil) // no WithSpiffe

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !def.called {
		t.Fatal("with a nil spiffe branch the default verifier must handle a non-STS credential")
	}
	if id.AgentID != "agent-a" || id.OrgID != "acme" {
		t.Fatalf("identity = %+v", id)
	}
}
