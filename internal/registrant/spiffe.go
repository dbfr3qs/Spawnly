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
}

// NewSpiffeVerifier returns a Verifier backed by v (normally a
// *spiffe.JWKSValidator).
func NewSpiffeVerifier(v spiffe.SVIDValidator) *SpiffeVerifier {
	return &SpiffeVerifier{Validator: v}
}

// Verify extracts a bearer JWT-SVID from the Authorization header, validates
// it against the registry audience, and derives AgentID as the last path
// segment of the SPIFFE ID (today's path.Base(spiffeID) behavior, moved
// here verbatim).
func (s *SpiffeVerifier) Verify(ctx context.Context, r *http.Request) (Identity, error) {
	rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if rawToken == "" {
		return Identity{}, errors.New("missing SVID")
	}
	spiffeID, err := s.Validator.Validate(ctx, rawToken, "registry")
	if err != nil {
		return Identity{}, err
	}
	return Identity{
		AgentID: path.Base(spiffeID),
		Subject: spiffeID,
		Issuer:  "spiffe-svid",
	}, nil
}
