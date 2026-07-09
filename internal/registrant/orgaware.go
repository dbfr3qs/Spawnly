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
		// Not an STS presentation → the default (SPIRE/OIDC/mTLS) path, unchanged.
		return v.def.Verify(ctx, r)
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
