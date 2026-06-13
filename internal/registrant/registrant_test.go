// internal/registrant/registrant_test.go
package registrant

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"

	"github.com/spawnly/platform/internal/spiffe"
)

// --- SpiffeVerifier -----------------------------------------------------

func TestSpiffeVerifier_DerivesAgentIDFromPathBase(t *testing.T) {
	v := NewSpiffeVerifier(&spiffe.MockSVIDValidator{
		SpiffeID: "spiffe://example.org/ns/default/sa/foo/bar",
	})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer test-svid")

	identity, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.AgentID != "bar" {
		t.Fatalf("AgentID = %q, want %q", identity.AgentID, "bar")
	}
	if identity.Subject != "spiffe://example.org/ns/default/sa/foo/bar" {
		t.Fatalf("Subject = %q", identity.Subject)
	}
	if identity.Issuer != "spiffe-svid" {
		t.Fatalf("Issuer = %q, want spiffe-svid", identity.Issuer)
	}
}

func TestSpiffeVerifier_MissingBearerToken(t *testing.T) {
	v := NewSpiffeVerifier(&spiffe.MockSVIDValidator{SpiffeID: "spiffe://example.org/agent/bar"})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error for missing bearer token")
	}
}

func TestSpiffeVerifier_ValidationError(t *testing.T) {
	v := NewSpiffeVerifier(&spiffe.MockSVIDValidator{Err: context.DeadlineExceeded})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer test-svid")
	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected validator error to propagate")
	}
}

// --- OIDCVerifier ---------------------------------------------------------

// testJWKS spins up an httptest server serving a JWKS for a freshly
// generated RSA key, and returns the server plus a signer for that key.
type testJWKS struct {
	server *httptest.Server
	key    jwk.Key // private key, used for signing
}

func newTestJWKS(t *testing.T) *testJWKS {
	t.Helper()

	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	privKey, err := jwk.FromRaw(raw)
	if err != nil {
		t.Fatalf("jwk.FromRaw: %v", err)
	}
	if err := privKey.Set(jwk.KeyIDKey, "test-key"); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := privKey.Set(jwk.AlgorithmKey, jwa.RS256); err != nil {
		t.Fatalf("set alg: %v", err)
	}

	pubKey, err := jwk.PublicKeyOf(privKey)
	if err != nil {
		t.Fatalf("public key: %v", err)
	}

	set := jwk.NewSet()
	if err := set.AddKey(pubKey); err != nil {
		t.Fatalf("add key to set: %v", err)
	}

	body, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	return &testJWKS{server: srv, key: privKey}
}

// sign builds and signs a JWT with the given claims using the test JWKS key.
func (j *testJWKS) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	b := jwt.NewBuilder()
	for k, v := range claims {
		b = b.Claim(k, v)
	}
	tok, err := b.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}
	signed, err := jwt.Sign(tok, jwt.WithKey(jwa.RS256, j.key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

func newOIDCVerifier(t *testing.T, jwks *testJWKS, cfg OIDCConfig) *OIDCVerifier {
	t.Helper()
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = jwks.server.URL
	}
	v, err := NewOIDCVerifier(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewOIDCVerifier: %v", err)
	}
	return v
}

func TestOIDCVerifier_ValidToken(t *testing.T) {
	jwks := newTestJWKS(t)
	v := newOIDCVerifier(t, jwks, OIDCConfig{
		Audience:     "registry",
		AgentIDClaim: "agent_id",
	})

	tok := jwks.sign(t, map[string]any{
		"sub":      "user-or-svc:123",
		"aud":      "registry",
		"agent_id": "agent-xyz",
		"exp":      time.Now().Add(time.Hour).Unix(),
	})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	identity, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.AgentID != "agent-xyz" {
		t.Fatalf("AgentID = %q, want agent-xyz", identity.AgentID)
	}
	if identity.Subject != "user-or-svc:123" {
		t.Fatalf("Subject = %q", identity.Subject)
	}
	if identity.Issuer != "oidc" {
		t.Fatalf("Issuer = %q, want oidc", identity.Issuer)
	}
}

func TestOIDCVerifier_DefaultAgentIDClaimIsSub(t *testing.T) {
	jwks := newTestJWKS(t)
	v := newOIDCVerifier(t, jwks, OIDCConfig{Audience: "registry"})

	tok := jwks.sign(t, map[string]any{
		"sub": "agent-from-sub",
		"aud": "registry",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	identity, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.AgentID != "agent-from-sub" {
		t.Fatalf("AgentID = %q, want agent-from-sub", identity.AgentID)
	}
}

func TestOIDCVerifier_WrongAudience(t *testing.T) {
	jwks := newTestJWKS(t)
	v := newOIDCVerifier(t, jwks, OIDCConfig{Audience: "registry"})

	tok := jwks.sign(t, map[string]any{
		"sub": "agent-1",
		"aud": "some-other-audience",
		"exp": time.Now().Add(time.Hour).Unix(),
	})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error for wrong audience")
	}
}

func TestOIDCVerifier_Expired(t *testing.T) {
	jwks := newTestJWKS(t)
	v := newOIDCVerifier(t, jwks, OIDCConfig{Audience: "registry"})

	tok := jwks.sign(t, map[string]any{
		"sub": "agent-1",
		"aud": "registry",
		"exp": time.Now().Add(-time.Hour).Unix(),
	})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestOIDCVerifier_MissingAgentIDClaim(t *testing.T) {
	jwks := newTestJWKS(t)
	v := newOIDCVerifier(t, jwks, OIDCConfig{
		Audience:     "registry",
		AgentIDClaim: "agent_id",
	})

	tok := jwks.sign(t, map[string]any{
		"sub": "agent-1",
		"aud": "registry",
		"exp": time.Now().Add(time.Hour).Unix(),
		// no "agent_id" claim
	})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.Header.Set("Authorization", "Bearer "+tok)

	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error for missing agent id claim")
	}
}

func TestOIDCVerifier_MissingBearerToken(t *testing.T) {
	jwks := newTestJWKS(t)
	v := newOIDCVerifier(t, jwks, OIDCConfig{Audience: "registry"})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error for missing bearer token")
	}
}

func TestNewOIDCVerifier_RequiresJWKSURL(t *testing.T) {
	if _, err := NewOIDCVerifier(context.Background(), OIDCConfig{Audience: "registry"}); err == nil {
		t.Fatal("expected error when JWKSURL is empty")
	}
}

// --- MTLSVerifier -----------------------------------------------------------

func certWithSANs(t *testing.T, uris []string, dnsNames []string, commonName string) *x509.Certificate {
	t.Helper()
	cert := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		DNSNames:     dnsNames,
	}
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil {
			t.Fatalf("parse uri %q: %v", u, err)
		}
		cert.URIs = append(cert.URIs, parsed)
	}
	return cert
}

func TestMTLSVerifier_SANURI(t *testing.T) {
	v := NewMTLSVerifier(MTLSConfig{AgentIDSource: MTLSSourceSANURI})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certWithSANs(t,
		[]string{"spiffe://cluster.local/ns/default/sa/foo/agent-abc"}, nil, "")}}

	identity, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.AgentID != "agent-abc" {
		t.Fatalf("AgentID = %q, want agent-abc", identity.AgentID)
	}
	if identity.Issuer != "mtls" {
		t.Fatalf("Issuer = %q, want mtls", identity.Issuer)
	}
}

func TestMTLSVerifier_SANDNS(t *testing.T) {
	v := NewMTLSVerifier(MTLSConfig{AgentIDSource: MTLSSourceSANDNS})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certWithSANs(t, nil, []string{"agent-dns.example"}, "")}}

	identity, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.AgentID != "agent-dns.example" {
		t.Fatalf("AgentID = %q", identity.AgentID)
	}
}

func TestMTLSVerifier_CommonName(t *testing.T) {
	v := NewMTLSVerifier(MTLSConfig{AgentIDSource: MTLSSourceCommonName})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certWithSANs(t, nil, nil, "agent-cn")}}

	identity, err := v.Verify(context.Background(), req)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if identity.AgentID != "agent-cn" {
		t.Fatalf("AgentID = %q", identity.AgentID)
	}
}

func TestMTLSVerifier_NoClientCert(t *testing.T) {
	v := NewMTLSVerifier(MTLSConfig{AgentIDSource: MTLSSourceSANURI})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error when no client certificate is presented")
	}
}

func TestMTLSVerifier_MissingSAN(t *testing.T) {
	v := NewMTLSVerifier(MTLSConfig{AgentIDSource: MTLSSourceSANURI})

	req := httptest.NewRequest("POST", "/v1/agents", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certWithSANs(t, nil, nil, "no-uri-here")}}

	if _, err := v.Verify(context.Background(), req); err == nil {
		t.Fatal("expected error for missing URI SAN")
	}
}
