// internal/tokenvalidator/validator.go
package tokenvalidator

import (
	"context"
	"fmt"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

type TokenValidator interface {
	ValidateAccessToken(ctx context.Context, token string) (spiffeID string, err error)
}

type JWKSValidator struct {
	issuer  string
	jwksURL string
	cache   *jwk.Cache
}

func New(ctx context.Context, issuer, jwksURL string) (*JWKSValidator, error) {
	cache := jwk.NewCache(ctx)
	if err := cache.Register(jwksURL); err != nil {
		return nil, err
	}
	if _, err := cache.Get(ctx, jwksURL); err != nil {
		return nil, fmt.Errorf("fetch IS JWKS: %w", err)
	}
	return &JWKSValidator{issuer: issuer, jwksURL: jwksURL, cache: cache}, nil
}

func (v *JWKSValidator) ValidateAccessToken(ctx context.Context, token string) (string, error) {
	keySet, err := v.cache.Get(ctx, v.jwksURL)
	if err != nil {
		return "", fmt.Errorf("get JWKS: %w", err)
	}
	tok, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(keySet),
		jwt.WithIssuer(v.issuer),
		jwt.WithValidate(true),
	)
	if err != nil {
		return "", fmt.Errorf("invalid access token: %w", err)
	}
	return tok.Subject(), nil
}

type MockValidator struct {
	SpiffeID string
	Err      error
}

func (m *MockValidator) ValidateAccessToken(_ context.Context, _ string) (string, error) {
	return m.SpiffeID, m.Err
}
