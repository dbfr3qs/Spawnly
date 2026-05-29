// internal/tokenvalidator/validator.go
package tokenvalidator

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// Claims is the parsed result of validating a delegated access token.
//
//   - User is the human user the token is on behalf of (the `sub`, e.g.
//     "user:<userId>").
//   - ActingAgent is the SPIFFE URI of the current actor (the outermost
//     `act.sub`). For backward-compat tokens with no `act`, it falls back to
//     `sub`.
//   - ActingAgentName is path.Base(ActingAgent) — the bare agent id.
//   - Chain is the full actor chain, outermost (current actor) first, each a
//     SPIFFE URI. Chain[0] == ActingAgent.
//   - Scopes are the granted scopes (parsed from `scope`, string or array).
//   - Audience is the token audience (`aud`, string or array).
//   - TokenUse is the optional `token_use` claim.
type Claims struct {
	User            string
	ActingAgent     string
	ActingAgentName string
	Chain           []string
	Scopes          []string
	Audience        []string
	TokenUse        string
}

// HasScope reports whether the given scope was granted.
func (c Claims) HasScope(s string) bool {
	for _, got := range c.Scopes {
		if got == s {
			return true
		}
	}
	return false
}

// HasAudience reports whether aud contains the given value.
func (c Claims) HasAudience(a string) bool {
	for _, got := range c.Audience {
		if got == a {
			return true
		}
	}
	return false
}

type TokenValidator interface {
	ValidateAccessToken(ctx context.Context, token string) (Claims, error)
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

func (v *JWKSValidator) ValidateAccessToken(ctx context.Context, token string) (Claims, error) {
	keySet, err := v.cache.Get(ctx, v.jwksURL)
	if err != nil {
		return Claims{}, fmt.Errorf("get JWKS: %w", err)
	}
	tok, err := jwt.Parse([]byte(token),
		jwt.WithKeySet(keySet),
		jwt.WithIssuer(v.issuer),
		jwt.WithValidate(true),
	)
	if err != nil {
		// The IdentityServer may have rotated its signing keys (e.g. on redeploy),
		// leaving our cached JWKS stale. Force a refresh and retry once so key
		// rotation doesn't require restarting resource servers.
		if refreshed, rerr := v.cache.Refresh(ctx, v.jwksURL); rerr == nil {
			if tok2, perr := jwt.Parse([]byte(token),
				jwt.WithKeySet(refreshed),
				jwt.WithIssuer(v.issuer),
				jwt.WithValidate(true),
			); perr == nil {
				return claimsFromToken(tok2), nil
			}
		}
		return Claims{}, fmt.Errorf("invalid access token: %w", err)
	}
	return claimsFromToken(tok), nil
}

// claimsFromToken extracts the delegation claims from a validated token.
// Parsing is intentionally defensive: every claim may be absent or in a
// different-than-expected shape (string vs array, etc.).
func claimsFromToken(tok jwt.Token) Claims {
	c := Claims{User: tok.Subject()}

	c.Audience = tok.Audience()

	if v, ok := tok.Get("scope"); ok {
		c.Scopes = parseSpaceOrArray(v)
	}
	if v, ok := tok.Get("token_use"); ok {
		if s, ok := v.(string); ok {
			c.TokenUse = s
		}
	}

	// Walk the nested `act` chain, outermost first.
	if v, ok := tok.Get("act"); ok {
		c.Chain = parseActChain(v)
	}

	if len(c.Chain) > 0 {
		c.ActingAgent = c.Chain[0]
	} else {
		// Backward-compat: tokens without `act` act as themselves.
		c.ActingAgent = c.User
	}
	c.ActingAgentName = path.Base(c.ActingAgent)
	return c
}

// parseActChain walks a possibly-nested `act` claim and returns the chain of
// actor subjects, outermost (current actor) first. Shape:
//
//	{ "sub": "<outer>", "act": { "sub": "<inner>", "act": {...} } }
func parseActChain(v any) []string {
	var chain []string
	for {
		m, ok := v.(map[string]any)
		if !ok {
			break
		}
		if sub, ok := m["sub"].(string); ok && sub != "" {
			chain = append(chain, sub)
		}
		next, ok := m["act"]
		if !ok {
			break
		}
		v = next
	}
	return chain
}

// parseSpaceOrArray handles a claim that may be a space-delimited string,
// a []string, or a []any of strings.
func parseSpaceOrArray(v any) []string {
	switch t := v.(type) {
	case string:
		return splitNonEmpty(t)
	case []string:
		var out []string
		for _, s := range t {
			out = append(out, splitNonEmpty(s)...)
		}
		return out
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, splitNonEmpty(s)...)
			}
		}
		return out
	}
	return nil
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		out = append(out, f)
	}
	return out
}

type MockValidator struct {
	// SpiffeID, if set, is used as the acting agent (and, when Claims is the
	// zero value, drives Chain/ActingAgent/ActingAgentName) for backward-compat
	// with existing tests.
	SpiffeID string
	Claims   Claims
	Err      error
}

func (m *MockValidator) ValidateAccessToken(_ context.Context, _ string) (Claims, error) {
	if m.Err != nil {
		return Claims{}, m.Err
	}
	c := m.Claims
	if m.SpiffeID != "" && c.ActingAgent == "" && len(c.Chain) == 0 {
		c.ActingAgent = m.SpiffeID
		c.ActingAgentName = path.Base(m.SpiffeID)
		c.Chain = []string{m.SpiffeID}
	}
	return c, nil
}
