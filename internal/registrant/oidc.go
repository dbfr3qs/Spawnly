package registrant

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// OIDCConfig configures an OIDCVerifier.
type OIDCConfig struct {
	// JWKSURL is the JWKS endpoint used to validate token signatures.
	// Required — automatic discovery via
	// <issuer>/.well-known/openid-configuration is a fast-follow.
	JWKSURL string

	// Audience is the expected "aud" claim.
	Audience string

	// AgentIDClaim names the JWT claim holding the agent identifier.
	// Defaults to "sub" if empty.
	AgentIDClaim string

	// InsecureSkipTLSVerify disables TLS verification when fetching the
	// JWKS — an opt-in escape hatch for self-hosted IdPs with self-signed
	// certs. Off by default, unlike spiffe.JWKSValidator's hardcoded
	// InsecureSkipVerify.
	InsecureSkipTLSVerify bool
}

// OIDCVerifier validates a generic OIDC JWT presented as a bearer token and
// derives Identity.AgentID from a configured claim.
type OIDCVerifier struct {
	cfg   OIDCConfig
	cache *jwk.Cache
}

// NewOIDCVerifier primes a JWKS cache for cfg.JWKSURL. It mirrors
// spiffe.NewJWKSValidator but keeps TLS verification on unless
// cfg.InsecureSkipTLSVerify is set.
func NewOIDCVerifier(ctx context.Context, cfg OIDCConfig) (*OIDCVerifier, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("OIDCConfig.JWKSURL is required")
	}
	if cfg.AgentIDClaim == "" {
		cfg.AgentIDClaim = "sub"
	}

	var registerOpts []jwk.RegisterOption
	if cfg.InsecureSkipTLSVerify {
		httpClient := &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			},
		}
		registerOpts = append(registerOpts, jwk.WithHTTPClient(httpClient))
	}

	cache := jwk.NewCache(ctx)
	if err := cache.Register(cfg.JWKSURL, registerOpts...); err != nil {
		return nil, err
	}
	if _, err := cache.Get(ctx, cfg.JWKSURL); err != nil {
		return nil, fmt.Errorf("fetch OIDC JWKS: %w", err)
	}
	return &OIDCVerifier{cfg: cfg, cache: cache}, nil
}

// Verify extracts a bearer JWT from the Authorization header, validates its
// signature/audience/standard claims against the configured JWKS, and
// derives AgentID from the configured agentIDClaim.
func (v *OIDCVerifier) Verify(ctx context.Context, r *http.Request) (Identity, error) {
	rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if rawToken == "" {
		return Identity{}, errors.New("missing bearer token")
	}

	keySet, err := v.cache.Get(ctx, v.cfg.JWKSURL)
	if err != nil {
		return Identity{}, fmt.Errorf("get JWKS: %w", err)
	}
	tok, err := jwt.Parse([]byte(rawToken),
		jwt.WithKeySet(keySet),
		jwt.WithAudience(v.cfg.Audience),
		jwt.WithValidate(true),
	)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid token: %w", err)
	}

	claimVal, ok := tok.Get(v.cfg.AgentIDClaim)
	if !ok {
		return Identity{}, fmt.Errorf("agent id claim %q missing", v.cfg.AgentIDClaim)
	}
	agentID, ok := claimVal.(string)
	if !ok || agentID == "" {
		return Identity{}, fmt.Errorf("agent id claim %q is not a non-empty string", v.cfg.AgentIDClaim)
	}

	return Identity{
		AgentID: agentID,
		Subject: tok.Subject(),
		Issuer:  "oidc",
	}, nil
}
