package controlplane

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

// OIDCConfig configures an OIDC control-plane Authenticator. It validates a
// client-credentials access token (e.g. minted by IdentityServer for the
// orchestrator/IdP clients) against a configured JWKS — never by calling a
// specific IdP, so the registry stays IdP-agnostic.
type OIDCConfig struct {
	// JWKSURL is the JWKS endpoint used to validate token signatures. Required.
	JWKSURL string
	// Audience is the expected "aud" claim (the registry's API-resource name).
	Audience string
	// RequiredScope, when set, must appear in the token's space-delimited or
	// array "scope" claim (e.g. "registry.consent"). Empty disables the check.
	RequiredScope string
	// InsecureSkipTLSVerify disables TLS verification when fetching the JWKS —
	// an opt-in escape hatch for self-hosted IdPs with self-signed certs.
	InsecureSkipTLSVerify bool
}

type oidcAuth struct {
	cfg   OIDCConfig
	cache *jwk.Cache
}

// NewOIDC primes a JWKS cache for cfg.JWKSURL and returns an OIDC control-plane
// Authenticator.
func NewOIDC(ctx context.Context, cfg OIDCConfig) (Authenticator, error) {
	if cfg.JWKSURL == "" {
		return nil, errors.New("OIDCConfig.JWKSURL is required")
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
		return nil, fmt.Errorf("fetch control-plane JWKS: %w", err)
	}
	return &oidcAuth{cfg: cfg, cache: cache}, nil
}

// Authenticate validates the bearer JWT's signature/audience/standard claims
// against the configured JWKS, enforces RequiredScope, and returns the caller's
// client_id and scopes.
func (o *oidcAuth) Authenticate(ctx context.Context, r *http.Request) (Caller, error) {
	rawToken := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if rawToken == "" {
		return Caller{}, errors.New("missing bearer token")
	}

	keySet, err := o.cache.Get(ctx, o.cfg.JWKSURL)
	if err != nil {
		return Caller{}, fmt.Errorf("get JWKS: %w", err)
	}
	tok, err := jwt.Parse([]byte(rawToken),
		jwt.WithKeySet(keySet),
		jwt.WithAudience(o.cfg.Audience),
		jwt.WithValidate(true),
	)
	if err != nil {
		return Caller{}, fmt.Errorf("invalid token: %w", err)
	}

	scopes := scopeClaim(tok)
	if o.cfg.RequiredScope != "" && !contains(scopes, o.cfg.RequiredScope) {
		return Caller{}, fmt.Errorf("token missing required scope %q", o.cfg.RequiredScope)
	}

	return Caller{ClientID: clientID(tok), Scopes: scopes}, nil
}

// scopeClaim normalizes the "scope" claim, which IdPs emit either as a
// space-delimited string (RFC 6749) or a JSON array (Duende's JWT default).
func scopeClaim(tok jwt.Token) []string {
	v, ok := tok.Get("scope")
	if !ok {
		return nil
	}
	switch s := v.(type) {
	case string:
		return strings.Fields(s)
	case []string:
		return s
	case []interface{}:
		out := make([]string, 0, len(s))
		for _, e := range s {
			if str, ok := e.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// clientID returns the OAuth client identifier, preferring the standard
// "client_id" claim and falling back to "sub" (which equals the client id for a
// client-credentials token).
func clientID(tok jwt.Token) string {
	if v, ok := tok.Get("client_id"); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return tok.Subject()
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
