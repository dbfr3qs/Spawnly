package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/registry"
	"github.com/spawnly/platform/internal/substrate"
	"github.com/spawnly/platform/internal/substrate/devca"
)

// writeDevCAFiles writes a fresh dev CA cert+key to a temp dir and returns paths.
func writeDevCAFiles(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	ca, err := devca.GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("gen ca: %v", err)
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatalf("key pem: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "ca.crt")
	keyPath = filepath.Join(dir, "ca.key")
	if err := os.WriteFile(certPath, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// clearIssuerEnv sets every CA-selection var to empty (== unset for the selector)
// so each case starts from a known state regardless of test ordering.
func clearIssuerEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"SUBSTRATE_KMS_KEY_ARN", "SUBSTRATE_CA_CERT_FILE", "SUBSTRATE_CA_KEY_FILE", "SPIRE_SERVER_SOCKET"} {
		t.Setenv(k, "")
	}
}

func newAPIForTest(t *testing.T) (*substrateAPI, error) {
	t.Helper()
	return newSubstrateAPI(registry.NewMock(nil), "http://registry.invalid", func() string { return "" }, nil)
}

// TestIssuerSelection_DevCA: cert+key files ⇒ dev-CA issuer wired (unchanged path).
func TestIssuerSelection_DevCA(t *testing.T) {
	clearIssuerEnv(t)
	certPath, keyPath := writeDevCAFiles(t)
	t.Setenv("SUBSTRATE_CA_CERT_FILE", certPath)
	t.Setenv("SUBSTRATE_CA_KEY_FILE", keyPath)

	api, err := newAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.issuer == nil {
		t.Fatal("dev CA path: issuer must be wired")
	}
	if _, ok := api.issuer.(*substrate.Issuer); !ok {
		t.Fatalf("expected *substrate.Issuer, got %T", api.issuer)
	}
}

// TestIssuerSelection_KMS: KMS ARN + cert-only ⇒ KMS issuer wired via the seam.
func TestIssuerSelection_KMS(t *testing.T) {
	clearIssuerEnv(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(certPath, []byte("dummy-cert-pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SUBSTRATE_KMS_KEY_ARN", "arn:aws:kms:us-east-1:1:key/abc")
	t.Setenv("SUBSTRATE_CA_CERT_FILE", certPath)

	// Stub the KMS seam so no real AWS is touched; assert it receives the cert PEM.
	var gotARN string
	var gotCert []byte
	orig := kmsIssuerFromEnv
	t.Cleanup(func() { kmsIssuerFromEnv = orig })
	kmsIssuerFromEnv = func(_ context.Context, keyARN string, caCertPEM []byte, _ string, _ time.Duration) (substrate.LeafIssuer, error) {
		gotARN = keyARN
		gotCert = caCertPEM
		return stubIssuer{}, nil
	}

	api, err := newAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.issuer == nil {
		t.Fatal("KMS path: issuer must be wired")
	}
	if gotARN != "arn:aws:kms:us-east-1:1:key/abc" || string(gotCert) != "dummy-cert-pem" {
		t.Fatalf("KMS seam got arn=%q cert=%q", gotARN, string(gotCert))
	}
}

// TestIssuerSelection_KMSFailsClosed: a KMS init error ⇒ nil issuer (503), NOT an
// error/panic and NOT a fallback to a weaker CA.
func TestIssuerSelection_KMSFailsClosed(t *testing.T) {
	clearIssuerEnv(t)
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	_ = os.WriteFile(certPath, []byte("x"), 0o600)
	t.Setenv("SUBSTRATE_KMS_KEY_ARN", "arn:kms:key")
	t.Setenv("SUBSTRATE_CA_CERT_FILE", certPath)

	orig := kmsIssuerFromEnv
	t.Cleanup(func() { kmsIssuerFromEnv = orig })
	kmsIssuerFromEnv = func(_ context.Context, _ string, _ []byte, _ string, _ time.Duration) (substrate.LeafIssuer, error) {
		return nil, context.DeadlineExceeded
	}

	api, err := newAPIForTest(t)
	if err != nil {
		t.Fatalf("KMS init failure must NOT be a hard error, got %v", err)
	}
	if api.issuer != nil {
		t.Fatal("KMS init failure must leave issuer nil (fail-safe 503)")
	}
}

// TestIssuerSelection_BothSetHardError: KMS ARN + dev key file ⇒ hard startup error.
func TestIssuerSelection_BothSetHardError(t *testing.T) {
	clearIssuerEnv(t)
	certPath, keyPath := writeDevCAFiles(t)
	t.Setenv("SUBSTRATE_KMS_KEY_ARN", "arn:kms:key")
	t.Setenv("SUBSTRATE_CA_CERT_FILE", certPath)
	t.Setenv("SUBSTRATE_CA_KEY_FILE", keyPath)

	// The seam must NOT even be consulted — the ambiguity is rejected first.
	orig := kmsIssuerFromEnv
	t.Cleanup(func() { kmsIssuerFromEnv = orig })
	called := false
	kmsIssuerFromEnv = func(_ context.Context, _ string, _ []byte, _ string, _ time.Duration) (substrate.LeafIssuer, error) {
		called = true
		return stubIssuer{}, nil
	}

	_, err := newAPIForTest(t)
	if err == nil {
		t.Fatal("both CAs configured must be a hard startup error")
	}
	if called {
		t.Fatal("both-set must be rejected before building any issuer")
	}
}

// TestIssuerSelection_NoneConfigured: neither var ⇒ nil issuer (connect 503).
func TestIssuerSelection_NoneConfigured(t *testing.T) {
	clearIssuerEnv(t)
	api, err := newAPIForTest(t)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if api.issuer != nil {
		t.Fatal("no CA configured must leave issuer nil")
	}
}

// stubIssuer is a no-op LeafIssuer for the selection-wiring tests.
type stubIssuer struct{}

func (stubIssuer) IssueOrgIdentity(string) (substrate.IssuedIdentity, error) {
	return substrate.IssuedIdentity{}, nil
}
func (stubIssuer) OrgID([]byte) (string, error) { return "", nil }
func (stubIssuer) TrustBundlePEM() []byte       { return nil }
