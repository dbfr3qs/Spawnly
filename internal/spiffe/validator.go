// internal/spiffe/validator.go
package spiffe

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

type SVIDValidator interface {
	Validate(ctx context.Context, token, audience string) (spiffeID string, err error)
}

type JWKSValidator struct {
	jwksURL string
	cache   *jwk.Cache
}

func NewJWKSValidator(ctx context.Context, jwksURL string) (*JWKSValidator, error) {
	// SPIRE OIDC provider uses a self-signed cert — skip verification for in-cluster fetches.
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	cache := jwk.NewCache(ctx)
	if err := cache.Register(jwksURL, jwk.WithHTTPClient(httpClient)); err != nil {
		return nil, err
	}
	if _, err := cache.Get(ctx, jwksURL); err != nil {
		return nil, fmt.Errorf("fetch SPIRE JWKS: %w", err)
	}
	return &JWKSValidator{jwksURL: jwksURL, cache: cache}, nil
}

func (v *JWKSValidator) Validate(ctx context.Context, token, audience string) (string, error) {
	keySet, err := v.cache.Get(ctx, v.jwksURL)
	if err != nil {
		return "", fmt.Errorf("get JWKS: %w", err)
	}
	tok, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(keySet),
		jwt.WithAudience(audience),
		jwt.WithValidate(true),
	)
	if err != nil {
		return "", fmt.Errorf("invalid SVID: %w", err)
	}
	return tok.Subject(), nil
}

type MockSVIDValidator struct {
	SpiffeID string
	Err      error
}

func (m *MockSVIDValidator) Validate(_ context.Context, _, _ string) (string, error) {
	return m.SpiffeID, m.Err
}
