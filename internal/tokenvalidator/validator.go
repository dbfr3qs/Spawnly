// internal/tokenvalidator/validator.go
package tokenvalidator

import (
	"context"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

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
//   - Roles are the user's role claims (parsed from `role`, string or array).
//     Used to gate admin-only operations (e.g. template management).
//   - TokenUse is the optional `token_use` claim.
type Claims struct {
	User            string
	ActingAgent     string
	ActingAgentName string
	Chain           []string
	Scopes          []string
	Audience        []string
	Roles           []string
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

// HasRole reports whether the given role was granted.
func (c Claims) HasRole(r string) bool {
	for _, got := range c.Roles {
		if got == r {
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
	// Bound both the initial fetch and background refreshes with a timeout-aware
	// HTTP client; otherwise a hung IdP socket blocks forever (the orchestrator's
	// buildSpawnValidator retry loop only recovers from errors, not a hang).
	if err := cache.Register(jwksURL, jwk.WithHTTPClient(&http.Client{Timeout: 10 * time.Second})); err != nil {
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

	// Role claim(s): a single role is a string ("admin"); multiple are a JSON
	// array. Both shapes are accepted so HasRole works regardless of how the
	// IdP serializes one vs many role claims.
	if v, ok := tok.Get("role"); ok {
		c.Roles = parseStringOrArray(v)
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

// parseStringOrArray handles a claim that may be a single string or an array of
// strings (e.g. "role"). Unlike parseSpaceOrArray it does NOT space-split, since
// role values are opaque identifiers. It returns defensive copies so a caller
// can't mutate the parsed slice and affect shared state.
func parseStringOrArray(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case []string:
		out := make([]string, len(t))
		copy(out, t)
		return out
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
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

// ── Transitional multi-issuer support (MIGRATION-TEMPORARY) ──────────────────
//
// REMOVE IN PHASE 6. During the Keycloak cutover the fleet's agent tokens are
// still minted by IdentityServer (issuer http://identity-server:8080) while
// human tokens are minted by Keycloak (issuer
// http://keycloak:8080/realms/spawnly). A resource server must accept BOTH for
// the duration of the migration. MultiIssuerValidator holds one JWKSValidator
// per (issuer, JWKS URL) pair and tries each in turn: the first that validates
// wins. Once Phase 6 flips every issuer to Keycloak and deletes IdentityServer,
// this type and NewMulti go away and callers revert to a single New().
type MultiIssuerValidator struct {
	validators []*JWKSValidator
}

// IssuerJWKS is one (issuer, JWKS URL) pair for NewMulti.
type IssuerJWKS struct {
	Issuer  string
	JWKSURL string
}

// NewMulti builds a validator that accepts tokens from any of the given
// (issuer, JWKS URL) pairs. It constructs one JWKSValidator per pair (each
// fetches its JWKS up front, so an unreachable IdP surfaces as an error the
// caller's retry loop recovers from). At least one pair is required.
//
// MIGRATION-TEMPORARY (see MultiIssuerValidator) — removed in Phase 6.
func NewMulti(ctx context.Context, pairs ...IssuerJWKS) (*MultiIssuerValidator, error) {
	if len(pairs) == 0 {
		return nil, fmt.Errorf("tokenvalidator: NewMulti requires at least one issuer")
	}
	m := &MultiIssuerValidator{}
	for _, p := range pairs {
		v, err := New(ctx, p.Issuer, p.JWKSURL)
		if err != nil {
			return nil, fmt.Errorf("tokenvalidator: issuer %q: %w", p.Issuer, err)
		}
		m.validators = append(m.validators, v)
	}
	return m, nil
}

// ValidateAccessToken validates the token against the configured issuers. It
// first ROUTES on the token's `iss` claim (parsed WITHOUT verification, purely
// to pick which validator to try) to the matching JWKSValidator, so a token is
// checked by exactly the validator that owns its issuer. Routing is an
// optimization, not a trust decision: the chosen JWKSValidator still fully
// verifies the signature AND re-checks `iss` via jwt.WithIssuer, so a forged
// `iss` cannot bypass validation — it just picks the (wrong) validator, which
// then rejects it.
//
// When routing MATCHES a validator (routed >= 0), we return that validator's
// result directly — success or failure. The other validators can never accept
// the token anyway: each enforces its own jwt.WithIssuer, so a token whose iss
// matched the routed validator is guaranteed to fail every other validator's
// issuer check. Trying them would be pure dead work that (a) recreates the very
// cross-issuer JWKS refresh-and-retry storm this routing exists to prevent, and
// (b) MASKS the routed validator's real error (e.g. `exp`) behind some other
// validator's issuer-mismatch. Only when routing finds NO match (routed == -1,
// e.g. iss absent/unparsed) do we try every validator as a fallback so a
// correct token is never dropped on a routing miss.
//
// Routing matters here because each JWKSValidator does a JWKS refresh-and-retry
// on failure (for key rotation): without it, every Keycloak token would force a
// spurious IdentityServer JWKS refresh (and vice-versa) on the losing
// validator, hammering the other IdP's JWKS endpoint. Widening issuers must not
// mean cross-issuer refresh storms.
//
// MIGRATION-TEMPORARY (see MultiIssuerValidator) — removed in Phase 6.
func (m *MultiIssuerValidator) ValidateAccessToken(ctx context.Context, token string) (Claims, error) {
	// Parse the `iss` WITHOUT verification just to route. A bad/forged iss only
	// misroutes to a validator that then rejects it — never accepts it.
	routed := -1
	if unverified, err := jwt.Parse([]byte(token), jwt.WithVerify(false), jwt.WithValidate(false)); err == nil {
		for i, v := range m.validators {
			if v.issuer == unverified.Issuer() {
				routed = i
				break
			}
		}
	}
	if routed >= 0 {
		// The routed validator OWNS this issuer; its result is authoritative. No
		// other validator can accept the token (each enforces a different issuer),
		// so return here — don't restart the cross-issuer refresh storm or mask
		// this validator's real error.
		return m.validators[routed].ValidateAccessToken(ctx, token)
	}
	// Routing miss (iss absent/unparsed/unknown): try every validator so a
	// correct token is never dropped, returning the last error otherwise.
	var lastErr error
	for _, v := range m.validators {
		c, err := v.ValidateAccessToken(ctx, token)
		if err == nil {
			return c, nil
		}
		lastErr = err
	}
	return Claims{}, lastErr
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
