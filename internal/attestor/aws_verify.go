package attestor

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// stsHostPattern matches the AWS STS endpoints (global or regional). The
// verifier replays the caller-supplied presigned request, so it MUST confirm
// the URL targets STS and nothing else — otherwise a caller could point the
// control plane at an arbitrary URL (SSRF).
var stsHostPattern = regexp.MustCompile(`^sts(\.[a-z0-9-]+)?\.amazonaws\.com$`)

// StsTrustedEndpointEnv, when set, names the SOLE STS endpoint the verifier will
// replay a presigned request against (scheme + host[:port] must match exactly).
//
// SSRF stance: this override NEVER widens trust. Unset (the production default)
// keeps the hardcoded AWS STS host pattern over HTTPS. When set, ONLY that one
// endpoint is trusted — the AWS pattern no longer applies, and any presigned URL
// pointing elsewhere is rejected. It exists solely so an in-cluster FAKE STS can
// stand in during e2e (see cmd/fake-sts); a real deploy leaves it UNSET, so no
// operator can be tricked into pointing the control plane at an attacker's host.
// Mirrors the Java StsCredentialVerifier STS_TRUSTED_ENDPOINT override so both
// verifiers agree.
const StsTrustedEndpointEnv = "STS_TRUSTED_ENDPOINT"

// getCallerIdentityResponse is the subset of the STS XML response we read.
type getCallerIdentityResponse struct {
	Arn     string `xml:"GetCallerIdentityResult>Arn"`
	Account string `xml:"GetCallerIdentityResult>Account"`
}

// CallerIdentity is the STS-attested principal derived from a replayed
// GetCallerIdentity response: the AWS account that owns the credential and the
// assumed-role ARN. The account is the field the anchor proof-of-control matches
// against (Phase 8); the ARN is matched against the anchor's role pattern.
type CallerIdentity struct {
	Account string
	Arn     string
}

// VerifyCallerIdentity replays a presigned GetCallerIdentity credential against
// AWS STS (or the configured trusted endpoint) and returns the attested account
// and ARN. AWS itself is the attestor: a valid SigV4 signature is required for
// STS to answer, so a successful response proves the caller held the role's
// credentials. This is the shared replay+parse core reused by both the
// registrant verifier (internal/registrant/aws.go) and the registry's anchor
// proof-of-control handler.
func VerifyCallerIdentity(ctx context.Context, httpClient *http.Client, credential string) (CallerIdentity, error) {
	pr, err := decodePresigned(credential)
	if err != nil {
		return CallerIdentity{}, err
	}
	if err := validateStsURL(pr.URL); err != nil {
		return CallerIdentity{}, err
	}
	return replayCallerIdentity(ctx, httpClient, pr)
}

// VerifyGetCallerIdentity replays a presigned GetCallerIdentity credential and
// returns just the attested caller ARN. Thin wrapper over VerifyCallerIdentity
// kept for the registrant verifier, which derives AgentID from the ARN session
// name and needs nothing else.
func VerifyGetCallerIdentity(ctx context.Context, httpClient *http.Client, credential string) (string, error) {
	id, err := VerifyCallerIdentity(ctx, httpClient, credential)
	if err != nil {
		return "", err
	}
	return id.Arn, nil
}

// replayCallerIdentity issues the presigned request and parses the attested
// account + ARN. It performs no host validation — VerifyCallerIdentity is the
// only caller and gates it behind validateStsURL first.
func replayCallerIdentity(ctx context.Context, httpClient *http.Client, pr presignedRequest) (CallerIdentity, error) {
	req, err := http.NewRequestWithContext(ctx, pr.Method, pr.URL, nil)
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("build STS request: %w", err)
	}
	for k, vs := range pr.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return CallerIdentity{}, fmt.Errorf("call STS GetCallerIdentity: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return CallerIdentity{}, fmt.Errorf("STS GetCallerIdentity returned %d: %s", resp.StatusCode, body)
	}

	var out getCallerIdentityResponse
	if err := xml.Unmarshal(body, &out); err != nil {
		return CallerIdentity{}, fmt.Errorf("parse STS response: %w", err)
	}
	if out.Arn == "" {
		return CallerIdentity{}, fmt.Errorf("STS response missing Arn")
	}
	account := out.Account
	if account == "" {
		// STS always returns Account, but derive it from the ARN as a fallback so
		// the proof-of-control match never depends on a single response field.
		account = AccountFromArn(out.Arn)
	}
	return CallerIdentity{Account: account, Arn: out.Arn}, nil
}

// validateStsURL rejects any presigned URL that does not target the trusted STS
// endpoint over the trusted scheme, and confirms it actually invokes
// GetCallerIdentity. It reads the STS_TRUSTED_ENDPOINT override from the
// environment; validateStsURLWith is the pure form used by tests.
func validateStsURL(raw string) error {
	return validateStsURLWith(raw, os.Getenv(StsTrustedEndpointEnv))
}

func validateStsURLWith(raw, trusted string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse presigned URL: %w", err)
	}
	if !strings.Contains(raw, "Action=GetCallerIdentity") {
		return fmt.Errorf("presigned request is not a GetCallerIdentity call")
	}
	if trusted != "" {
		// Override active: the presigned URL must be EXACTLY the trusted endpoint
		// (scheme + host + port). Never widened — the AWS host pattern below does
		// not apply, so a set override can only ever NARROW trust to one endpoint.
		return matchTrustedEndpoint(u, trusted)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("presigned URL must be https, got %q", u.Scheme)
	}
	if !stsHostPattern.MatchString(u.Hostname()) {
		return fmt.Errorf("presigned URL host %q is not an AWS STS endpoint", u.Hostname())
	}
	return nil
}

// matchTrustedEndpoint requires u to share the trusted endpoint's scheme and
// host authority (host + port). The trusted endpoint may be plain http (the
// in-cluster fake STS); that relaxation is deliberate and scoped to whatever the
// operator explicitly configured — it is never inferred.
func matchTrustedEndpoint(u *url.URL, trusted string) error {
	t, err := url.Parse(trusted)
	if err != nil || t.Scheme == "" || t.Host == "" {
		return fmt.Errorf("invalid %s %q", StsTrustedEndpointEnv, trusted)
	}
	if !strings.EqualFold(u.Scheme, t.Scheme) || !strings.EqualFold(u.Host, t.Host) {
		return fmt.Errorf("presigned URL is not the trusted STS endpoint")
	}
	return nil
}

// SessionNameFromArn extracts the RoleSessionName from an assumed-role ARN
// (arn:aws:sts::<acct>:assumed-role/<role>/<sessionName>). The operator sets the
// session name to the agentId, so this is the AgentId derivation — kept in lock
// step with the C# StsCredentialVerifier so both halves agree (the consistency
// invariant).
func SessionNameFromArn(arn string) (string, error) {
	// .../assumed-role/<role>/<sessionName>
	const marker = ":assumed-role/"
	i := strings.Index(arn, marker)
	if i < 0 {
		return "", fmt.Errorf("ARN %q is not an assumed-role ARN", arn)
	}
	rest := arn[i+len(marker):]
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		return "", fmt.Errorf("ARN %q has no session name", arn)
	}
	return parts[1], nil
}

// MatchRoleArnPattern reports whether an assumed-role ARN matches an anchor's
// roleArnPattern. The pattern is a simple glob: `*` matches any run of
// characters (including none); every other character is literal. Matching is
// anchored (the whole ARN must match) and case-sensitive. This is the anchor
// proof-of-control / verifier role check — kept here beside the STS helpers so
// the Phase-9 Go org-aware verifier reuses the exact same semantics. An empty
// pattern matches nothing (fail-closed): an anchor with no role constraint would
// otherwise activate against any role in the account.
func MatchRoleArnPattern(pattern, arn string) bool {
	if pattern == "" || arn == "" {
		return false
	}
	// Translate the glob to an anchored regexp, escaping every non-`*` char.
	var b strings.Builder
	b.WriteString("^")
	for i, seg := range strings.Split(pattern, "*") {
		if i > 0 {
			b.WriteString(".*")
		}
		b.WriteString(regexp.QuoteMeta(seg))
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(arn)
}

// AccountFromArn extracts the 12-digit account id from an ARN
// (arn:partition:service:region:<account>:resource). Returns "" if the ARN has
// no account segment. Used only as a fallback when the STS response omits the
// Account field.
func AccountFromArn(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return ""
	}
	return parts[4]
}
