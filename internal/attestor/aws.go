package attestor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// AwsStsAssertionType is the OAuth client-assertion-type URN under which an
// AWS STS GetCallerIdentity credential is presented. It is NOT a JWT, so it
// carries its own type rather than reusing JWTBearerAssertionType.
const AwsStsAssertionType = "urn:spawnly:params:aws-sts-getcalleridentity"

// presignedRequest is the wire form of a SigV4-presigned GetCallerIdentity
// request: enough for a verifier to replay it against AWS STS, which then
// attests the caller's assumed-role ARN. Modeled on HashiCorp Vault's AWS IAM
// auth method.
type presignedRequest struct {
	Method  string              `json:"method"`
	URL     string              `json:"url"`
	Headers map[string][]string `json:"headers,omitempty"`
}

// AwsSource fetches an AWS STS attestation credential. Under EKS IRSA the
// default credential chain resolves the pod's web-identity token
// (AWS_WEB_IDENTITY_TOKEN_FILE) and assumes AWS_ROLE_ARN with
// AWS_ROLE_SESSION_NAME (set by the operator to the agentId). The resulting
// temporary credentials are used to presign a GetCallerIdentity request, which
// is the credential the control plane verifies.
//
// The audience argument is ignored: GetCallerIdentity has no audience concept,
// and the same credential serves both registry self-registration and IS token
// minting (so both derive the same AgentId — the session name).
type AwsSource struct {
	presign func(ctx context.Context) (presignedRequest, error)
}

// AwsEndpointURLStsEnv overrides the STS endpoint the presigner targets. It is
// the standard AWS SDK service-endpoint variable name; honouring it explicitly
// lets the CLIENT side (the sidecar's presign source, cmd/anchorctl) point a
// presigned GetCallerIdentity at an in-cluster FAKE STS for e2e, matching the
// verifier's STS_TRUSTED_ENDPOINT override. Unset in a real deploy, so the
// presigner uses the ambient/regional AWS STS endpoint as before.
const AwsEndpointURLStsEnv = "AWS_ENDPOINT_URL_STS"

// awsRoleSessionNameEnv is the standard AWS role-session-name variable. In a
// real IRSA/AssumeRole deploy the SDK bakes it into the assumed-role identity, so
// GetCallerIdentity returns an ARN whose session name IS this value (== agentId).
// The fake STS performs no role assumption, so it cannot learn the session name
// from the presigned request. See the fakeSessionParam affordance below.
const awsRoleSessionNameEnv = "AWS_ROLE_SESSION_NAME"

// fakeSessionParam is an E2E-ONLY query parameter the presign source appends when
// (and ONLY when) AWS_ENDPOINT_URL_STS points the presigner at the in-cluster
// fake STS. It carries AWS_ROLE_SESSION_NAME so the fake can echo it as the
// returned ARN's session name — faithfully modelling AWS, where the CALLER
// chooses the role-session-name (the agentId) while the account+role come from
// the anchor. It is appended AFTER SigV4 signing, which is why it is gated to
// fake mode: a real STS would reject the now-unsigned extra param, but the fake
// ignores SigV4 entirely (see cmd/fake-sts). A real deploy never sets
// AWS_ENDPOINT_URL_STS, so this param is never emitted.
const fakeSessionParam = "X-Fake-Session"

// NewAwsSource builds an AwsSource from the ambient AWS configuration. It does
// not require credentials at construction time — they are resolved lazily on
// the first Fetch (IRSA-friendly).
func NewAwsSource(ctx context.Context) (*AwsSource, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	var stsOpts []func(*sts.Options)
	fakeEndpoint := os.Getenv(AwsEndpointURLStsEnv)
	if fakeEndpoint != "" {
		// BaseEndpoint routes the presigned request at the configured endpoint
		// (e.g. the fake STS). The signature still uses real SigV4; the fake
		// deliberately ignores it (see cmd/fake-sts).
		stsOpts = append(stsOpts, func(o *sts.Options) { o.BaseEndpoint = aws.String(fakeEndpoint) })
	}
	// Only in fake mode (endpoint override set) do we carry the session name as an
	// out-of-band query param the fake echoes as the ARN session name. Never in a
	// real deploy.
	fakeSession := ""
	if fakeEndpoint != "" {
		fakeSession = os.Getenv(awsRoleSessionNameEnv)
	}
	presigner := sts.NewPresignClient(sts.NewFromConfig(cfg, stsOpts...))
	return &AwsSource{
		presign: func(ctx context.Context) (presignedRequest, error) {
			req, err := presigner.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return presignedRequest{}, fmt.Errorf("presign GetCallerIdentity: %w", err)
			}
			return presignedRequest{Method: req.Method, URL: appendFakeSession(req.URL, fakeSession), Headers: req.SignedHeader}, nil
		},
	}, nil
}

// appendFakeSession appends the E2E-ONLY X-Fake-Session query param to a presigned
// URL when session is non-empty (fake mode only — see fakeSessionParam). It is a
// no-op otherwise, so the real-STS URL is returned byte-for-byte unchanged.
func appendFakeSession(rawURL, session string) string {
	if session == "" {
		return rawURL
	}
	sep := "&"
	if !strings.Contains(rawURL, "?") {
		sep = "?"
	}
	return rawURL + sep + fakeSessionParam + "=" + url.QueryEscape(session)
}

// Fetch implements Source.
func (s *AwsSource) Fetch(ctx context.Context, _ string) (Credential, error) {
	pr, err := s.presign(ctx)
	if err != nil {
		return Credential{}, err
	}
	enc, err := encodePresigned(pr)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Value: enc, AssertionType: AwsStsAssertionType}, nil
}

func encodePresigned(pr presignedRequest) (string, error) {
	b, err := json.Marshal(pr)
	if err != nil {
		return "", fmt.Errorf("marshal presigned request: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func decodePresigned(credential string) (presignedRequest, error) {
	b, err := base64.StdEncoding.DecodeString(credential)
	if err != nil {
		return presignedRequest{}, fmt.Errorf("base64-decode credential: %w", err)
	}
	var pr presignedRequest
	if err := json.Unmarshal(b, &pr); err != nil {
		return presignedRequest{}, fmt.Errorf("unmarshal presigned request: %w", err)
	}
	return pr, nil
}
