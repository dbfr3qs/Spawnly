// internal/tokenvalidator/multi_test.go
//
// Tests for the MIGRATION-TEMPORARY multi-issuer validator (removed in Phase 6).
package tokenvalidator

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
)

// testIssuer is a fake IdP: an RSA signing key + an httptest server that serves
// its public JWKS. It can mint signed tokens for a given issuer string.
type testIssuer struct {
	issuer  string
	jwksURL string
	priv    jwk.Key // RSA private key with a kid
	srv     *httptest.Server
}

// newTestIssuer spins up a JWKS endpoint for a fresh RSA key. The issuer string
// is what the minted tokens carry in `iss` and what the validator checks.
func newTestIssuer(t *testing.T, issuer string) *testIssuer {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen rsa: %v", err)
	}
	priv, err := jwk.FromRaw(raw)
	if err != nil {
		t.Fatalf("jwk from raw: %v", err)
	}
	_ = priv.Set(jwk.KeyIDKey, "test-key-1")
	_ = priv.Set(jwk.AlgorithmKey, jwa.RS256)

	pub, err := priv.PublicKey()
	if err != nil {
		t.Fatalf("public key: %v", err)
	}
	set := jwk.NewSet()
	_ = set.AddKey(pub)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		buf, _ := json.Marshal(set) // jwk.Set implements json.Marshaler → public JWKS
		_, _ = w.Write(buf)
	}))
	t.Cleanup(srv.Close)

	return &testIssuer{issuer: issuer, jwksURL: srv.URL, priv: priv, srv: srv}
}

// mint signs a token carrying the issuer + the given extra claims.
func (ti *testIssuer) mint(t *testing.T, claims map[string]any) string {
	t.Helper()
	b := jwt.NewBuilder().
		Issuer(ti.issuer).
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(time.Hour))
	for k, v := range claims {
		b = b.Claim(k, v)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, ti.priv))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

// TestMultiIssuerAcceptsBothIssuers proves the dual-issuer shim accepts a token
// from EITHER configured issuer (the whole point of the Keycloak cutover: agent
// tokens from IdentityServer + human tokens from Keycloak both validate).
func TestMultiIssuerAcceptsBothIssuers(t *testing.T) {
	ctx := context.Background()
	is := newTestIssuer(t, "http://identity-server:8080")
	kc := newTestIssuer(t, "http://keycloak:8080/realms/spawnly")

	v, err := NewMulti(ctx,
		IssuerJWKS{Issuer: is.issuer, JWKSURL: is.jwksURL},
		IssuerJWKS{Issuer: kc.issuer, JWKSURL: kc.jwksURL},
	)
	if err != nil {
		t.Fatalf("NewMulti: %v", err)
	}

	// A token from the IdentityServer issuer validates.
	isTok := is.mint(t, map[string]any{"sub": "user:agent-1", "scope": "sample-api-a:read"})
	c, err := v.ValidateAccessToken(ctx, isTok)
	if err != nil {
		t.Fatalf("IS token rejected: %v", err)
	}
	if c.User != "user:agent-1" || !c.HasScope("sample-api-a:read") {
		t.Errorf("IS claims wrong: %+v", c)
	}

	// A token from the Keycloak realm issuer ALSO validates, with the flat role.
	kcTok := kc.mint(t, map[string]any{"sub": "user:alice", "role": "admin", "aud": "orchestrator"})
	c, err = v.ValidateAccessToken(ctx, kcTok)
	if err != nil {
		t.Fatalf("Keycloak token rejected: %v", err)
	}
	if c.User != "user:alice" || !c.HasRole("admin") || !c.HasAudience("orchestrator") {
		t.Errorf("Keycloak claims wrong: %+v", c)
	}
}

// TestMultiIssuerRejectsUnknownIssuer proves a token whose issuer matches NEITHER
// configured pair is rejected — widening the accepted issuer set must not turn
// into accepting arbitrary issuers.
func TestMultiIssuerRejectsUnknownIssuer(t *testing.T) {
	ctx := context.Background()
	is := newTestIssuer(t, "http://identity-server:8080")
	kc := newTestIssuer(t, "http://keycloak:8080/realms/spawnly")
	rogue := newTestIssuer(t, "http://evil.example/realms/spawnly")

	v, err := NewMulti(ctx,
		IssuerJWKS{Issuer: is.issuer, JWKSURL: is.jwksURL},
		IssuerJWKS{Issuer: kc.issuer, JWKSURL: kc.jwksURL},
	)
	if err != nil {
		t.Fatalf("NewMulti: %v", err)
	}

	rogueTok := rogue.mint(t, map[string]any{"sub": "user:mallory", "role": "admin"})
	if _, err := v.ValidateAccessToken(ctx, rogueTok); err == nil {
		t.Fatal("token from an unconfigured issuer was accepted")
	}
}

// TestMultiIssuerRejectsWrongSignature proves a token that carries a CONFIGURED
// issuer string but is signed by the WRONG key (the other issuer's key) is
// rejected — the shim widens issuers, not signature trust.
func TestMultiIssuerRejectsWrongSignature(t *testing.T) {
	ctx := context.Background()
	is := newTestIssuer(t, "http://identity-server:8080")
	kc := newTestIssuer(t, "http://keycloak:8080/realms/spawnly")

	v, err := NewMulti(ctx,
		IssuerJWKS{Issuer: is.issuer, JWKSURL: is.jwksURL},
		IssuerJWKS{Issuer: kc.issuer, JWKSURL: kc.jwksURL},
	)
	if err != nil {
		t.Fatalf("NewMulti: %v", err)
	}

	// Mint a token that CLAIMS the Keycloak issuer but sign it with the IS key.
	// Routing sends it to the Keycloak validator (iss matches), which rejects it
	// on signature — the wrong-signature path we want to exercise.
	b := jwt.NewBuilder().
		Issuer(kc.issuer).
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(time.Hour)).
		Claim("sub", "user:mallory").
		Claim("role", "admin")
	tok, _ := b.Build()
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, is.priv))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := v.ValidateAccessToken(ctx, string(signed)); err == nil {
		t.Fatal("Keycloak-issuer token signed by the IdentityServer key was accepted")
	}
}

// mintExpired signs a token whose exp is already in the past (still a well-formed
// iss so it routes) — used to exercise the routed-validator-rejects path.
func (ti *testIssuer) mintExpired(t *testing.T, claims map[string]any) string {
	t.Helper()
	b := jwt.NewBuilder().
		Issuer(ti.issuer).
		IssuedAt(time.Now().Add(-2 * time.Hour)).
		Expiration(time.Now().Add(-1 * time.Hour))
	for k, v := range claims {
		b = b.Claim(k, v)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, ti.priv))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

// TestMultiIssuerRoutedErrorIsReturnedDirectly proves that when the token's iss
// matches a configured validator but that validator rejects it (here: expired),
// the routed validator's own error is returned — NOT masked by another
// validator's issuer-mismatch. This is the finding #6 fix: a matched route must
// short-circuit rather than fall through to the try-all loop (which would both
// mask the real error and re-trigger cross-issuer JWKS refreshes).
func TestMultiIssuerRoutedErrorIsReturnedDirectly(t *testing.T) {
	ctx := context.Background()
	is := newTestIssuer(t, "http://identity-server:8080")
	kc := newTestIssuer(t, "http://keycloak:8080/realms/spawnly")

	v, err := NewMulti(ctx,
		IssuerJWKS{Issuer: is.issuer, JWKSURL: is.jwksURL},
		IssuerJWKS{Issuer: kc.issuer, JWKSURL: kc.jwksURL},
	)
	if err != nil {
		t.Fatalf("NewMulti: %v", err)
	}

	// Expired token from the Keycloak issuer, correctly signed by the KC key: it
	// routes to the KC validator, which rejects it on exp. The error surfaced must
	// mention exp, not the IS validator's issuer mismatch.
	expired := kc.mintExpired(t, map[string]any{"sub": "alice"})
	_, err = v.ValidateAccessToken(ctx, expired)
	if err == nil {
		t.Fatal("expired token was accepted")
	}
	if !strings.Contains(err.Error(), "exp") {
		t.Errorf("routed error was masked: got %q, want an exp/expiration error", err.Error())
	}
}

// TestNewMultiRequiresAtLeastOnePair guards the API contract.
func TestNewMultiRequiresAtLeastOnePair(t *testing.T) {
	if _, err := NewMulti(context.Background()); err == nil {
		t.Fatal("NewMulti with no pairs should error")
	}
}
