package registrant

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/spawnly/platform/internal/spiffe"
)

// SpiffeVerifier wraps an spiffe.SVIDValidator and adapts its output to
// Identity. This is the default Verifier, preserving the registry's
// pre-Phase-3 behavior. It is the only file in this package allowed to
// import internal/spiffe — it's the adapter, per the platform's
// dependency-direction rule.
type SpiffeVerifier struct {
	Validator spiffe.SVIDValidator

	// TrustDomain is the expected SPIFFE trust domain. When set (the production
	// default is "cluster.local", wired from SPIRE_TRUST_DOMAIN in
	// cmd/registry/main.go), Verify parses the validated SVID through the
	// canonical spiffe.ParsePath contract, REJECTS any SVID whose trust domain
	// differs, and surfaces the org segment on Identity.OrgID so the
	// OrgAwareVerifier can bind it to the record's org.
	//
	// When EMPTY (the zero value, used by many existing unit tests that mint
	// SVIDs in arbitrary trust domains like "example.org"), the verifier stays
	// in legacy passthrough mode: AgentID = path.Base(spiffeID) and OrgID = "",
	// bit-identical to the pre-substrate behavior. A cluster deployed without
	// SPIRE_TRUST_DOMAIN still gets "cluster.local" from the env default, so
	// production always enforces.
	TrustDomain string
}

// NewSpiffeVerifier returns a Verifier backed by v (normally a
// *spiffe.JWKSValidator) with trust-domain enforcement OFF (legacy
// passthrough). Production wiring sets TrustDomain after construction; the
// bare constructor keeps every existing call site's behavior unchanged.
func NewSpiffeVerifier(v spiffe.SVIDValidator) *SpiffeVerifier {
	return &SpiffeVerifier{Validator: v}
}

// Verify extracts a bearer JWT-SVID from the Authorization header, validates
// it against the registry audience, and derives the caller Identity.
//
// AgentID is the last path segment of the SPIFFE ID (path.Base(spiffeID)),
// unchanged from the original behavior in BOTH modes. When TrustDomain is
// configured, the SVID is additionally parsed through the canonical path
// contract: a trust-domain mismatch or a malformed org-scoped path is rejected
// (fail closed), and a valid org segment is surfaced on Identity.OrgID.
func (s *SpiffeVerifier) Verify(ctx context.Context, r *http.Request) (Identity, error) {
	rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if rawToken == "" {
		return Identity{}, errors.New("missing SVID")
	}
	spiffeID, err := s.Validator.Validate(ctx, rawToken, "registry")
	if err != nil {
		return Identity{}, err
	}

	// Legacy passthrough: no expected trust domain configured ⇒ exactly today's
	// behavior (path.Base, no org). Kept so pre-substrate deployments and the
	// many trust-domain-agnostic unit tests are unaffected.
	if s.TrustDomain == "" {
		return Identity{
			AgentID: path.Base(spiffeID),
			Subject: spiffeID,
			Issuer:  "spiffe-svid",
		}, nil
	}

	// Enforcing mode: parse through the canonical contract, pin the trust
	// domain, and surface the org segment. Any parse failure (incl. the
	// "/org//agent/…" empty-org spoof and other malformed org paths) is a
	// generic rejection — fail closed.
	parsed, err := spiffe.ParsePath(spiffeID)
	if err != nil {
		return Identity{}, err
	}
	if parsed.TrustDomain != s.TrustDomain {
		return Identity{}, errors.New("SVID trust domain not permitted")
	}
	return Identity{
		AgentID: parsed.AgentID, // == path.Base(spiffeID), contract-preserved
		OrgID:   parsed.OrgID,
		Subject: spiffeID,
		Issuer:  "spiffe-svid",
		// A genuinely platform (org-less) path in ENFORCING mode: mark it so the
		// OrgAwareVerifier applies the reverse org-binding (a platform SVID must not
		// authenticate a substrate-placed record). Never set in legacy passthrough.
		platformSVID: parsed.OrgID == "",
	}, nil
}
