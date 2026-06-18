package registrant

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/spawnly/platform/internal/attestor"
)

// AwsStsVerifier authenticates a registration request whose bearer token is an
// AWS STS GetCallerIdentity credential. It replays the credential against STS
// and derives AgentID from the assumed-role session name. Like spiffe.go, this
// is the adapter that bridges a concrete attestor mechanism (internal/attestor)
// to the neutral Identity, per the dependency-direction rule.
type AwsStsVerifier struct {
	httpClient *http.Client
}

// NewAwsStsVerifier returns a Verifier backed by AWS STS. A nil httpClient uses
// http.DefaultClient.
func NewAwsStsVerifier(httpClient *http.Client) *AwsStsVerifier {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &AwsStsVerifier{httpClient: httpClient}
}

// Verify extracts the presigned GetCallerIdentity credential from the
// Authorization header, replays it against STS, and derives AgentID from the
// session name in the returned ARN.
func (v *AwsStsVerifier) Verify(ctx context.Context, r *http.Request) (Identity, error) {
	cred := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if cred == "" {
		return Identity{}, errors.New("missing AWS STS credential")
	}
	arn, err := attestor.VerifyGetCallerIdentity(ctx, v.httpClient, cred)
	if err != nil {
		return Identity{}, err
	}
	agentID, err := attestor.SessionNameFromArn(arn)
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		AgentID: agentID,
		Subject: arn,
		Issuer:  "aws-sts",
	}, nil
}
