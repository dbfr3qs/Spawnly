package registrant

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/spiffe"
)

// The SPIRE two-org isolation matrix (remote-substrate Phase 1, spec AC 4/5),
// mirroring the STS matrix in orgaware_test.go but for org-scoped JWT-SVIDs.
// The full chain under test is the real one: a trust-domain-enforcing
// SpiffeVerifier (parses the signed path, surfaces OrgID) wrapped by
// OrgAwareVerifier (binds OrgID to the record's org, fail-closed).

const spireTD = "cluster.local"

// spireVerifier builds OrgAwareVerifier over a real SpiffeVerifier whose
// MockSVIDValidator returns spiffeID for any token, with trust-domain
// enforcement pinned to cluster.local.
func spireVerifier(spiffeID string, store orgStore) *OrgAwareVerifier {
	sv := NewSpiffeVerifier(&spiffe.MockSVIDValidator{SpiffeID: spiffeID})
	sv.TrustDomain = spireTD
	return NewOrgAwareVerifier(sv, store, nil)
}

// svidRequest presents a non-STS bearer credential so OrgAwareVerifier routes
// to the SPIRE (default) branch; the MockSVIDValidator ignores the token value.
func svidRequest() *http.Request {
	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer header.payload.signature")
	return req
}

func twoOrgStore() *stubStore {
	return &stubStore{
		agents: map[string]registry.AgentRecord{
			// agent-a / agent-b are PLATFORM-placed org records (an org member's
			// agent that runs on the platform — the pre-substrate world). Placement
			// is left unset (== PlacementK8s), the platform-hosted default.
			"agent-a":   {AgentID: "agent-a", OrgID: "acme"},
			"agent-b":   {AgentID: "agent-b", OrgID: "globex"},
			"orgless-1": {AgentID: "orgless-1", OrgID: ""},
			// sub-a is a SUBSTRATE-placed org record: it may be authenticated ONLY by
			// org acme's org-scoped substrate SVID, never by an org-less platform SVID.
			"sub-a": {AgentID: "sub-a", OrgID: "acme", Placement: registry.PlacementSubstrate},
		},
		orgs: map[string]registry.Organization{
			"acme":   {OrgID: "acme"},
			"globex": {OrgID: "globex"},
		},
	}
}

// org-A SVID × org-A record ⇒ verified, OrgID surfaced, org store consulted.
func TestSpireOrg_MatchesOwnOrg_Verified(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/org/acme/agent/t/alice/worker/agent-a", twoOrgStore())

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.AgentID != "agent-a" || id.OrgID != "acme" || id.Issuer != "spiffe-svid" {
		t.Fatalf("identity = %+v", id)
	}
}

// org-A SVID × org-B record ⇒ refused (the record it names is owned by globex).
func TestSpireOrg_CrossOrgRecord_Refused(t *testing.T) {
	// SVID claims org acme but its agentId (path.Base) names agent-b, an org-B record.
	v := spireVerifier("spiffe://cluster.local/org/acme/agent/t/alice/worker/agent-b", twoOrgStore())

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected refusal for a cross-org SVID/record pairing")
	}
}

// org-scoped SVID × org-less record ⇒ refused (no org on the record to match).
func TestSpireOrg_OrgLessRecord_Refused(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/org/acme/agent/t/alice/worker/orgless-1", twoOrgStore())

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected refusal for an org-scoped SVID against an org-less record")
	}
}

// org-scoped SVID × unknown record ⇒ refused.
func TestSpireOrg_UnknownRecord_Refused(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/org/acme/agent/t/alice/worker/ghost", twoOrgStore())

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected refusal for an unknown record")
	}
}

// REGRESSION: an org-less (platform) SVID authenticates against a platform
// (org-less) record exactly as today. Note: since the reverse org-binding fix
// (Phase 3) the org-less path DOES consult the store once to confirm the named
// record is not substrate-placed — a platform SVID must never authenticate a
// substrate-placed record — but a platform-placed record still passes through.
func TestSpireOrg_PlatformSvid_Regression(t *testing.T) {
	store := twoOrgStore()
	v := spireVerifier("spiffe://cluster.local/agent/weather/orgless-1", store)

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.AgentID != "orgless-1" || id.OrgID != "" {
		t.Fatalf("identity = %+v", id)
	}
}

// REGRESSION (spec AC 9): an org-LESS (platform) SVID MUST authenticate an
// org-scoped record that is PLATFORM-placed — that is exactly how an org member's
// platform-hosted agent works (the platform ClusterSPIFFEID mints org-less paths
// while the member's spawn preregisters a record carrying their OrgID). The
// reverse-binding refusal is now placement-aware and must NOT fire here.
func TestSpireOrg_PlatformSvidVsPlatformPlacedOrgRecord_Allowed(t *testing.T) {
	// Platform SVID whose path.Base is "agent-a", an ACME (org-scoped) PLATFORM
	// record (Placement unset == k8s).
	v := spireVerifier("spiffe://cluster.local/agent/weather/agent-a", twoOrgStore())

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("a platform SVID must authenticate a PLATFORM-placed org record (AC 9): %v", err)
	}
	if id.AgentID != "agent-a" || id.OrgID != "" {
		t.Fatalf("identity = %+v (org-less SVID must stay org-less)", id)
	}
}

// PRESERVED INVARIANT: an org-LESS (platform) SVID must NOT authenticate a
// SUBSTRATE-placed record — that record may be claimed only by its org-scoped
// substrate SVID (`…/org/{orgId}/agent/…`). Otherwise a malicious platform pod
// could label itself with a substrate agent's agentId and mint for it.
func TestSpireOrg_PlatformSvidVsSubstrateRecord_Refused(t *testing.T) {
	// Platform SVID whose path.Base is "sub-a", an ACME SUBSTRATE-placed record.
	v := spireVerifier("spiffe://cluster.local/agent/weather/sub-a", twoOrgStore())

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("a platform SVID must not authenticate a substrate-placed record")
	}
}

// An org-less SVID naming an UNKNOWN record passes through (it may name a record
// that self-registers later — today's behaviour), so platform agents are
// unaffected.
func TestSpireOrg_PlatformSvidUnknownRecord_Passthrough(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/agent/weather/ghost", twoOrgStore())

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.OrgID != "" || id.AgentID != "ghost" {
		t.Fatalf("identity = %+v", id)
	}
}

// A store OUTAGE on the org-less path PASSES THROUGH (preserving platform-agent
// availability): the reverse-binding breach can only complete if the mint then
// loads the SUBSTRATE record, which needs the store up — so an outage can never
// turn this passthrough into an authenticated substrate record. Uses sub-a (the
// substrate record) so the outage is the ONLY thing preventing the refusal.
func TestSpireOrg_PlatformSvidStoreOutage_Passthrough(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/agent/weather/sub-a", erroringStore{twoOrgStore()})

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("org-less store outage should pass through, got %v", err)
	}
	if id.OrgID != "" {
		t.Fatalf("identity = %+v", id)
	}
}

// Wrong trust domain ⇒ refused (fail closed) even for an otherwise well-formed
// org-scoped path.
func TestSpireOrg_WrongTrustDomain_Refused(t *testing.T) {
	v := spireVerifier("spiffe://evil.example/org/acme/agent/t/alice/worker/agent-a", twoOrgStore())

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected refusal for a wrong trust domain")
	}
}

// Malformed org segment (the "/org//agent/…" empty-org spoof) ⇒ refused before
// any store lookup.
func TestSpireOrg_EmptyOrgSpoof_Refused(t *testing.T) {
	store := twoOrgStore()
	v := spireVerifier("spiffe://cluster.local/org//agent/agent-a", store)

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected refusal for the /org//agent spoof")
	}
	if store.getAgents != 0 {
		t.Fatal("a malformed path must be rejected before touching the store")
	}
}

// A path where "org" appears deeper is treated as platform (no org claim), NOT
// as an org smuggle — so it binds as an org-less identity.
func TestSpireOrg_OrgInSubpath_TreatedAsPlatform(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/agent/org/orgless-1", twoOrgStore())

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.OrgID != "" || id.AgentID != "orgless-1" {
		t.Fatalf("identity = %+v (an org sub-path must not become an org claim)", id)
	}
}

// erroringStore fails GetAgent to exercise the fail-closed store-outage path.
type erroringStore struct{ orgStore }

func (erroringStore) GetAgent(context.Context, string) (registry.AgentRecord, error) {
	return registry.AgentRecord{}, errors.New("dynamodb throttled")
}

// GetAgent outage on the org-bound path ⇒ fail closed.
func TestSpireOrg_StoreOutage_FailsClosed(t *testing.T) {
	v := spireVerifier("spiffe://cluster.local/org/acme/agent/t/alice/worker/agent-a",
		erroringStore{twoOrgStore()})

	if _, err := v.Verify(context.Background(), svidRequest()); err == nil {
		t.Fatal("expected a GetAgent outage to fail closed")
	}
}

// Trust-domain enforcement OFF (legacy zero value) ⇒ bit-identical passthrough:
// even an org-scoped path yields OrgID="" and AgentID=path.Base, and the store
// is untouched (the OrgAwareVerifier sees no org to bind).
func TestSpireOrg_LegacyPassthrough_NoEnforcement(t *testing.T) {
	store := twoOrgStore()
	sv := NewSpiffeVerifier(&spiffe.MockSVIDValidator{
		SpiffeID: "spiffe://example.org/org/acme/agent/t/u/worker/agent-a",
	}) // TrustDomain left empty
	v := NewOrgAwareVerifier(sv, store, nil)

	id, err := v.Verify(context.Background(), svidRequest())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if id.OrgID != "" || id.AgentID != "agent-a" {
		t.Fatalf("identity = %+v, want legacy passthrough (no org, path.Base id)", id)
	}
	if store.getAgents != 0 {
		t.Fatal("legacy passthrough must not touch the org store")
	}
}
