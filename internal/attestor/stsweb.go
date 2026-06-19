package attestor

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// DefaultStsWebAudience is the JWT `aud` requested for the web-identity token —
// the platform control plane that validates it. Both the registry and the
// IdentityServer verifiers must expect this value.
const DefaultStsWebAudience = "spawnly"

// StsWebSource fetches an AWS STS web-identity token via the outbound
// web-identity federation API (sts:GetWebIdentityToken). Under EKS Pod Identity
// the returned AWS-signed JWT carries cluster-attested principal_tags
// (kubernetes-pod-name, ...) that the verifier reads to derive the agent id.
//
// No caller Tags are passed: identity must come from the EKS-set principal_tags
// in the session, never from caller-supplied request_tags (which a workload
// could forge). Credentials are resolved from the EKS Pod Identity agent via the
// default credential chain.
type StsWebSource struct {
	audience string
	// fetch returns a freshly vended web-identity token. Injectable for tests.
	fetch func(ctx context.Context) (string, error)
}

// NewStsWebSource builds an StsWebSource. audience becomes the token's `aud`
// (defaults to DefaultStsWebAudience when empty). AWS credentials are resolved
// lazily on first Fetch (Pod Identity-friendly).
func NewStsWebSource(ctx context.Context, audience string) (*StsWebSource, error) {
	if audience == "" {
		audience = DefaultStsWebAudience
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	client := sts.NewFromConfig(cfg)
	return &StsWebSource{
		audience: audience,
		fetch: func(ctx context.Context) (string, error) {
			out, err := client.GetWebIdentityToken(ctx, &sts.GetWebIdentityTokenInput{
				Audience:         []string{audience},
				SigningAlgorithm: aws.String("RS256"),
				DurationSeconds:  aws.Int32(3600),
			})
			if err != nil {
				return "", fmt.Errorf("GetWebIdentityToken: %w", err)
			}
			if out.WebIdentityToken == nil {
				return "", fmt.Errorf("GetWebIdentityToken returned no token")
			}
			return *out.WebIdentityToken, nil
		},
	}, nil
}

// Fetch implements Source. The audience argument is ignored — the token's `aud`
// is fixed at construction so both control-plane verifiers can pin it.
func (s *StsWebSource) Fetch(ctx context.Context, _ string) (Credential, error) {
	tok, err := s.fetch(ctx)
	if err != nil {
		return Credential{}, err
	}
	return Credential{Value: tok, AssertionType: JWTBearerAssertionType}, nil
}
