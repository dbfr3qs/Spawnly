package attestor

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// stsHostPattern matches the AWS STS endpoints (global or regional). The
// verifier replays the caller-supplied presigned request, so it MUST confirm
// the URL targets STS and nothing else — otherwise a caller could point the
// control plane at an arbitrary URL (SSRF).
var stsHostPattern = regexp.MustCompile(`^sts(\.[a-z0-9-]+)?\.amazonaws\.com$`)

// getCallerIdentityResponse is the subset of the STS XML response we read.
type getCallerIdentityResponse struct {
	Arn string `xml:"GetCallerIdentityResult>Arn"`
}

// VerifyGetCallerIdentity replays a presigned GetCallerIdentity credential
// against AWS STS and returns the attested caller ARN. AWS itself is the
// attestor here: a valid SigV4 signature is required for STS to answer, so a
// successful response proves the caller held the role's credentials.
func VerifyGetCallerIdentity(ctx context.Context, httpClient *http.Client, credential string) (string, error) {
	pr, err := decodePresigned(credential)
	if err != nil {
		return "", err
	}
	if err := validateStsURL(pr.URL); err != nil {
		return "", err
	}
	return replayGetCallerIdentity(ctx, httpClient, pr)
}

// replayGetCallerIdentity issues the presigned request and parses the attested
// ARN. It performs no host validation — VerifyGetCallerIdentity is the only
// caller and gates it behind validateStsURL first.
func replayGetCallerIdentity(ctx context.Context, httpClient *http.Client, pr presignedRequest) (string, error) {
	req, err := http.NewRequestWithContext(ctx, pr.Method, pr.URL, nil)
	if err != nil {
		return "", fmt.Errorf("build STS request: %w", err)
	}
	for k, vs := range pr.Headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call STS GetCallerIdentity: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("STS GetCallerIdentity returned %d: %s", resp.StatusCode, body)
	}

	var out getCallerIdentityResponse
	if err := xml.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("parse STS response: %w", err)
	}
	if out.Arn == "" {
		return "", fmt.Errorf("STS response missing Arn")
	}
	return out.Arn, nil
}

// validateStsURL rejects any presigned URL that does not target an AWS STS
// endpoint over HTTPS, and confirms it actually invokes GetCallerIdentity.
func validateStsURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse presigned URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("presigned URL must be https, got %q", u.Scheme)
	}
	if !stsHostPattern.MatchString(u.Hostname()) {
		return fmt.Errorf("presigned URL host %q is not an AWS STS endpoint", u.Hostname())
	}
	if !strings.Contains(raw, "Action=GetCallerIdentity") {
		return fmt.Errorf("presigned request is not a GetCallerIdentity call")
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
