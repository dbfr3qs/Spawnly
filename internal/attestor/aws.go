package attestor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

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

// NewAwsSource builds an AwsSource from the ambient AWS configuration. It does
// not require credentials at construction time — they are resolved lazily on
// the first Fetch (IRSA-friendly).
func NewAwsSource(ctx context.Context) (*AwsSource, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	presigner := sts.NewPresignClient(sts.NewFromConfig(cfg))
	return &AwsSource{
		presign: func(ctx context.Context) (presignedRequest, error) {
			req, err := presigner.PresignGetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
			if err != nil {
				return presignedRequest{}, fmt.Errorf("presign GetCallerIdentity: %w", err)
			}
			return presignedRequest{Method: req.Method, URL: req.URL, Headers: req.SignedHeader}, nil
		},
	}, nil
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
