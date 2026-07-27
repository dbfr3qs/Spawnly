package registrant

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/spawnly/platform/internal/attestor"
	"github.com/spawnly/platform/internal/registry"
)

// orgStore is the narrow slice of registry.Store the org-aware verifier needs to
// resolve an agent's owning organization and its attestation anchor. Declared
// here (not importing the whole Store) so the dependency points from this
// adapter toward the platform's neutral contract, never the reverse.
type orgStore interface {
	GetAgent(ctx context.Context, id string) (registry.AgentRecord, error)
	GetOrg(ctx context.Context, id string) (registry.Organization, bool, error)
}

// OrgAwareVerifier is the registry-side mirror of the Keycloak
// OrgAwareVerifier: it wraps the deployment's configured default registrant
// Verifier and adds per-org STS resolution so a consumer-hosted (remote) agent
// can self-register with an AWS STS credential while platform SPIRE pods are
// untouched.
//
// THE ISOLATION INVARIANT (spec criterion 12): an STS credential verifies ONLY
// against the anchor of the org that owns the agent's registry record. The
// AWS-attested account MUST equal that org's anchor account and the assumed-role
// ARN MUST match the anchor's role pattern. Org A's AWS account can never
// register an agent recorded under org B; an unanchored org's agents can never
// ride any anchor; there is NO fallback across verifiers. Every failure returns
// a plain error the handler renders as an identical 401 (oracle-free — the
// caller cannot distinguish unknown-agent from wrong-org from bad-credential).
//
// SPIRE default path is BIT-IDENTICAL: a non-STS credential (a JWT-SVID, an OIDC
// token, an mTLS cert) is routed straight to the wrapped default verifier with
// no extra work, so platform agents behave exactly as before.
type OrgAwareVerifier struct {
	def        Verifier     // the configured default (SPIFFE/OIDC/mTLS/aws-sts)
	spiffe     Verifier     // OPTIONAL substrate-edge SPIFFE verifier (see WithSpiffe); nil elsewhere
	store      orgStore     // registry.Store — resolves record → org → anchor
	httpClient *http.Client // STS replay client (bounded)
}

// NewOrgAwareVerifier wraps def with per-org STS resolution backed by store. A
// nil httpClient gets a bounded default so a hung STS socket can never wedge the
// registration path.
func NewOrgAwareVerifier(def Verifier, store orgStore, httpClient *http.Client) *OrgAwareVerifier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &OrgAwareVerifier{def: def, store: store, httpClient: httpClient}
}

// WithSpiffe installs a SEPARATE SPIFFE verifier used ONLY for the non-STS
// (SPIFFE/OIDC/mTLS) branch of Verify, and returns the receiver for chaining.
//
// WHY this exists: the AWS substrate edge is the ONE deployment that needs BOTH
// registrant verifiers at once. Platform EKS agents authenticate with
// aws-stsweb — so REGISTRANT_VERIFIER defaults to aws-stsweb and `def` is the
// stsweb verifier — but a substrate workload presents a SPIFFE JWT-SVID that is
// NOT STS-shaped and that the stsweb verifier would reject (the confirmed 401
// on self-register). Wiring a dedicated SPIFFE verifier here routes those SVIDs
// to a real SPIFFE validator while leaving the STS path (and the org-less STS
// fallback to `def`) untouched.
//
// When nil (every other deployment, including kind where `def` IS already the
// spiffe-svid verifier) the non-STS branch keeps using `def`, so behaviour is
// bit-identical to before. This is opt-in only, selected in cmd/registry/main.go
// by REGISTRANT_SUBSTRATE_VERIFIER=spiffe-svid.
func (v *OrgAwareVerifier) WithSpiffe(spiffe Verifier) *OrgAwareVerifier {
	v.spiffe = spiffe
	return v
}

// Verify routes by credential SHAPE: an AWS STS presigned GetCallerIdentity
// envelope takes the org-bound STS path; anything else (a JWT-SVID, an OIDC
// token, an mTLS request) is delegated verbatim to the default verifier.
//
// Peek-before-bind for STS: the RoleSessionName that IS the agentId lives only
// in the STS *response*, never in the presigned request, so the STS replay is
// itself the peek — it yields the AWS-attested (account, arn, sessionName). We
// then look up the record for that agentId and bind the attested account+arn to
// THAT record's org anchor. The agentId is AWS-attested (not a caller claim), so
// binding it to the record's anchor is exactly the isolation check.
func (v *OrgAwareVerifier) Verify(ctx context.Context, r *http.Request) (Identity, error) {
	cred := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !looksLikeStsCredential(cred) {
		// Not an STS presentation → a SPIFFE/OIDC/mTLS verifier.
		//
		// SUBSTRATE-EDGE COEXISTENCE (spec AC-5a): on the AWS substrate edge the
		// DEFAULT verifier (v.def) is aws-stsweb — correct for platform EKS agents —
		// but a substrate workload authenticates with a SPIFFE JWT-SVID that stsweb
		// would reject. When a dedicated SPIFFE verifier has been wired (WithSpiffe),
		// the non-STS branch uses IT to validate the SVID; otherwise it falls back to
		// v.def unchanged. On kind, v.def already IS the spiffe verifier and v.spiffe
		// stays nil, so this is bit-identical to before. The STS path below, and the
		// org-less STS fallback to v.def, are NOT affected by v.spiffe.
		nonSts := v.def
		if v.spiffe != nil {
			nonSts = v.spiffe
		}
		id, err := nonSts.Verify(ctx, r)
		if err != nil {
			return Identity{}, err
		}
		return v.bindSpireOrg(ctx, id)
	}

	// STS presentation: replay against the trusted STS endpoint (org-independent —
	// the same endpoint for every org; STS_TRUSTED_ENDPOINT narrows it to the
	// in-cluster fake STS in e2e). AWS is the attestor.
	id, err := attestor.VerifyCallerIdentity(ctx, v.httpClient, cred)
	if err != nil {
		return Identity{}, err
	}
	agentID, err := attestor.SessionNameFromArn(id.Arn)
	if err != nil {
		return Identity{}, err
	}

	rec, err := v.store.GetAgent(ctx, agentID)
	if err != nil {
		// Store error propagates as a plain error — the handler renders the same
		// oracle-free 401 as any other failure; never fail-open.
		return Identity{}, err
	}
	if rec.AgentID == "" {
		return Identity{}, errors.New("unknown agent for STS credential")
	}
	if rec.OrgID == "" {
		// Org-less agent: no org anchor governs it, so the platform default
		// verifier makes the authoritative decision (preserves the pluggable-
		// attestation aws-sts deployment, where the default IS the STS verifier;
		// on a SPIRE-default platform the default rejects the STS credential).
		return v.def.Verify(ctx, r)
	}

	org, ok, err := v.store.GetOrg(ctx, rec.OrgID)
	if err != nil {
		// getOrg outage ⇒ fail CLOSED (never mint against an unknown anchor).
		return Identity{}, err
	}
	anchor := org.Anchor
	if !ok || anchor == nil || anchor.Status != registry.AnchorActive || anchor.Type != "sts" {
		// Unanchored (or pending) org: an STS credential can never ride an anchor
		// that is not active — no fallback to the default verifier.
		return Identity{}, errors.New("org has no active STS anchor")
	}
	// Bind the AWS-attested identity to THIS record's org anchor (the invariant).
	if id.Account == "" || id.Account != anchor.AWSAccountID {
		return Identity{}, errors.New("STS account is not the agent's org anchor account")
	}
	if !attestor.MatchRoleArnPattern(anchor.RoleArnPattern, id.Arn) {
		return Identity{}, errors.New("STS role ARN does not match the org anchor pattern")
	}
	return Identity{AgentID: agentID, Subject: id.Arn, Issuer: "aws-sts"}, nil
}

// bindSpireOrg is the SPIRE-side mirror of the STS org-binding: it sits beside
// verifyStsOrgBound with the SAME fail-closed discipline. An org-scoped SVID
// (Identity.OrgID != "", set by the trust-domain-enforcing SpiffeVerifier from
// the signed path) authenticates ONLY against a record whose OrgID equals the
// SVID's org — mismatch, an org-less record, an unknown record, or a store
// outage is a generic rejection, never a fallback.
//
// An org-LESS identity (OrgID == "") is platform-hosted (or a non-SPIFFE
// default verifier that never sets OrgID) and passes through bit-identical to
// the pre-substrate behavior — the org store is never touched.
func (v *OrgAwareVerifier) bindSpireOrg(ctx context.Context, id Identity) (Identity, error) {
	if id.OrgID == "" {
		// REVERSE org-binding (remote-substrate carry-forward), now PLACEMENT-AWARE:
		// the refusal exists so a genuinely platform (org-less) SVID cannot claim a
		// record that is supposed to be authenticated only by an ORG-SCOPED substrate
		// SVID. But an org-less (platform) SVID is exactly how an org MEMBER's
		// PLATFORM-hosted agent authenticates: the platform ClusterSPIFFEID mints
		// org-LESS paths (`/agent/{tenant}/{user}/{type}/{id}`), while that member's
		// spawn preregisters a record carrying the member's OrgID. That pairing is
		// the pre-substrate world and MUST keep working (spec AC 9: "orgs without a
		// substrate behave exactly as today").
		//
		// So refuse ONLY when the record's placement REQUIRES an org-scoped identity —
		// PlacementSubstrate. A substrate-placed record must be claimed solely by its
		// org-scoped substrate SVID (`…/org/{orgId}/agent/…`); an org-less SVID
		// authenticating one would be the reverse-binding breach the check guards.
		// For PlacementK8s / PlacementRemote / unset the org-less SVID IS the
		// legitimate platform path, so it passes through. Placement is server-set at
		// preregistration and immutable (a self-registering sidecar never sets it from
		// its body — enforced in cmd/registry/main.go's self-register handler, which
		// re-reads existing.Placement), so a caller cannot game which branch it lands in.
		//
		// This fires ONLY for id.platformSVID — a trust-domain-verified `/agent/…`
		// path in enforcing mode. In legacy passthrough (enforcement off) or for a
		// non-SPIFFE default verifier the flag is unset, so behaviour is
		// bit-identical to before (no store lookup, no new refusal).
		//
		// A store OUTAGE (err != nil) PASSES THROUGH, preserving today's
		// platform-agent availability (org-less SVIDs never depended on the store):
		// the reverse-binding breach can only COMPLETE if the mint then loads the
		// substrate record, which needs the store up — so an outage can never turn
		// this passthrough into an authenticated substrate record. A non-substrate
		// record (platform-placed, org-scoped or not) also passes through.
		if id.platformSVID {
			rec, err := v.store.GetAgent(ctx, id.AgentID)
			// Refuse on placement ALONE — not also gated on rec.OrgID != "". A
			// substrate-placed record must be claimed only by its org-scoped substrate
			// SVID, period; requiring a non-empty org too would (in the theoretical
			// case of an org-less substrate record, which the orchestrator never
			// creates) let a platform SVID claim it. Placement-only is strictly more
			// fail-closed. Keeps the Go and Java sides symmetric.
			if err == nil && rec.AgentID != "" && rec.Placement == registry.PlacementSubstrate {
				return Identity{}, errors.New("platform SVID cannot authenticate a substrate-placed record")
			}
		}
		return id, nil
	}
	// Org-scoped SVID: bind the signed-path org to the record's org. The
	// agentID is SVID-attested (path.Base of the signed path), so requiring the
	// record it names to carry the SAME org is exactly the isolation check — an
	// org-A SVID can never authenticate an agent recorded under org B.
	rec, err := v.store.GetAgent(ctx, id.AgentID)
	if err != nil {
		// Store error ⇒ fail CLOSED (the handler renders the same oracle-free
		// 401 as any other failure).
		return Identity{}, err
	}
	if rec.AgentID == "" || rec.OrgID == "" || rec.OrgID != id.OrgID {
		// Unknown record, org-less record (no org to match a scoped SVID), or a
		// cross-org record ⇒ generic reject.
		return Identity{}, errors.New("SVID org does not match the agent record's org")
	}
	return id, nil
}

// looksLikeStsCredential reports whether cred is a base64-encoded presigned
// GetCallerIdentity envelope (method + url). A JWT-SVID contains '.' separators
// that fail standard base64 decoding, so this never misclassifies a SPIRE
// credential — the SPIRE path stays bit-identical. A misclassification could
// only ever cause a rejection, never a bypass (the STS path still replays and
// binds; the default path still verifies the signature).
func looksLikeStsCredential(cred string) bool {
	if cred == "" {
		return false
	}
	b, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		return false
	}
	var pr struct {
		Method string `json:"method"`
		URL    string `json:"url"`
	}
	if err := json.Unmarshal(b, &pr); err != nil {
		return false
	}
	return pr.Method != "" && pr.URL != ""
}
