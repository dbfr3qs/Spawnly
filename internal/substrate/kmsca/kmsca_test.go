package kmsca

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/spawnly/platform/internal/substrate"
	"github.com/spawnly/platform/internal/substrate/devca"
)

// fakeKMS is an in-process stand-in for the KMS API surface, backed by a real
// ECDSA P-256 key. It proves the issuer end-to-end with NO real AWS: GetPublicKey
// returns the DER SPKI; Sign hashes-nothing (the caller passes a digest) and
// returns an ASN.1 DER ECDSA signature — exactly the KMS contract.
type fakeKMS struct {
	key   *ecdsa.PrivateKey
	usage kmstypes.KeyUsageType
	algs  []kmstypes.SigningAlgorithmSpec
	signs int
}

func newFakeKMS(t *testing.T) *fakeKMS {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen ecdsa: %v", err)
	}
	return &fakeKMS{
		key:   key,
		usage: kmstypes.KeyUsageTypeSignVerify,
		algs:  []kmstypes.SigningAlgorithmSpec{kmstypes.SigningAlgorithmSpecEcdsaSha256},
	}
}

func (f *fakeKMS) GetPublicKey(_ context.Context, _ *kms.GetPublicKeyInput, _ ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error) {
	der, err := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if err != nil {
		return nil, err
	}
	return &kms.GetPublicKeyOutput{
		PublicKey:         der,
		KeyUsage:          f.usage,
		SigningAlgorithms: f.algs,
		KeySpec:           kmstypes.KeySpecEccNistP256,
	}, nil
}

func (f *fakeKMS) Sign(_ context.Context, in *kms.SignInput, _ ...func(*kms.Options)) (*kms.SignOutput, error) {
	f.signs++
	if in.MessageType != kmstypes.MessageTypeDigest {
		return nil, errTest("Sign must use MessageType=DIGEST")
	}
	if in.SigningAlgorithm != kmstypes.SigningAlgorithmSpecEcdsaSha256 {
		return nil, errTest("Sign must use ECDSA_SHA_256")
	}
	sig, err := ecdsa.SignASN1(rand.Reader, f.key, in.Message)
	if err != nil {
		return nil, err
	}
	return &kms.SignOutput{Signature: sig}, nil
}

type errTest string

func (e errTest) Error() string { return string(e) }

const testTD = "spawnly.run"

func newKMSIssuer(t *testing.T, f *fakeKMS) *Issuer {
	t.Helper()
	ctx := context.Background()
	signer, err := NewSigner(ctx, f, "arn:aws:kms:us-east-1:1234:key/abc")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	caCertPEM, err := BootstrapCACert(signer, nil, time.Hour)
	if err != nil {
		t.Fatalf("BootstrapCACert: %v", err)
	}
	iss, err := NewIssuer(ctx, f, "arn:aws:kms:us-east-1:1234:key/abc", caCertPEM, testTD, time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return iss
}

// TestIssuedLeaf_VerifiesAgainstCACert proves a KMS-issued leaf chains to the KMS
// CA cert (the exact check the SPIRE x509pop attestor performs).
func TestIssuedLeaf_VerifiesAgainstCACert(t *testing.T) {
	f := newFakeKMS(t)
	iss := newKMSIssuer(t, f)
	issued, err := iss.IssueOrgIdentity("acme")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	caCert, err := parseCert(iss.TrustBundlePEM())
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	leaf, err := parseCert(issued.Leaf.CertPEM)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("leaf does not verify against KMS CA: %v", err)
	}
	if f.signs == 0 {
		t.Fatal("expected at least one kms:Sign call for issuance")
	}
}

// TestIssuedLeaf_CNIsNormalizedOrg proves the org binding: CN == normalized org.
func TestIssuedLeaf_CNIsNormalizedOrg(t *testing.T) {
	iss := newKMSIssuer(t, newFakeKMS(t))
	issued, err := iss.IssueOrgIdentity("Acme") // mixed case must normalize
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	org, err := iss.OrgID(issued.Leaf.CertPEM)
	if err != nil {
		t.Fatalf("read org: %v", err)
	}
	if org != "acme" {
		t.Fatalf("CN org = %q, want acme", org)
	}
}

// TestNodeID_MatchesDevCADerivation proves the x509pop node id of a KMS-issued
// leaf is derived through the SAME funnel as the dev CA: same org prefix, and the
// fingerprint segment is SHA-1 of THIS leaf's DER (devca.NodeIDFromLeaf). This is
// what guarantees node→org attestation semantics are unchanged across CAs.
func TestNodeID_MatchesDevCADerivation(t *testing.T) {
	kmsIss := newKMSIssuer(t, newFakeKMS(t))
	kmsIssued, err := kmsIss.IssueOrgIdentity("acme")
	if err != nil {
		t.Fatalf("kms issue: %v", err)
	}

	// A dev-CA-issued equivalent for the same org + trust domain.
	devCA, err := devca.GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("dev ca: %v", err)
	}
	devIss, err := substrate.NewIssuer(devCA, testTD, time.Hour)
	if err != nil {
		t.Fatalf("dev issuer: %v", err)
	}
	devIssued, err := devIss.IssueOrgIdentity("acme")
	if err != nil {
		t.Fatalf("dev issue: %v", err)
	}

	// Same org prefix (the org half of the node id) from both CAs.
	prefix, _ := devca.NodeSpiffePathPrefix(testTD, "acme")
	if !strings.HasPrefix(kmsIssued.NodeID, prefix) {
		t.Fatalf("kms node id %q not under %q", kmsIssued.NodeID, prefix)
	}
	if !strings.HasPrefix(devIssued.NodeID, prefix) {
		t.Fatalf("dev node id %q not under %q", devIssued.NodeID, prefix)
	}

	// Same derivation: NodeIDFromLeaf recomputed on the KMS leaf equals the issued
	// NodeID (proving both went through devca.NodeIDFromLeaf, not a divergent path).
	recomputed, err := devca.NodeIDFromLeaf(testTD, kmsIssued.Leaf.CertPEM)
	if err != nil {
		t.Fatalf("recompute node id: %v", err)
	}
	if recomputed != kmsIssued.NodeID {
		t.Fatalf("kms node id %q != recomputed %q — derivation diverged", kmsIssued.NodeID, recomputed)
	}
}

// TestTTL_Honored proves the issued leaf's NotAfter honors the configured TTL.
func TestTTL_Honored(t *testing.T) {
	f := newFakeKMS(t)
	ctx := context.Background()
	signer, _ := NewSigner(ctx, f, "k")
	caCertPEM, _ := BootstrapCACert(signer, nil, time.Hour)
	ttl := 42 * time.Minute
	iss, err := NewIssuer(ctx, f, "k", caCertPEM, testTD, ttl)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}
	issued, err := iss.IssueOrgIdentity("acme")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	want := time.Now().Add(ttl)
	if issued.ExpiresAt.Before(want.Add(-5*time.Minute)) || issued.ExpiresAt.After(want.Add(5*time.Minute)) {
		t.Fatalf("ExpiresAt %v not ~%v (ttl %v)", issued.ExpiresAt, want, ttl)
	}
}

// TestNewIssuer_RefusesSPKIMismatchCACert proves a CA cert whose key is NOT the
// KMS key is refused — the issuer can never sign leaves that chain to a foreign CA.
func TestNewIssuer_RefusesSPKIMismatchCACert(t *testing.T) {
	f := newFakeKMS(t)
	ctx := context.Background()

	// A CA cert from a DIFFERENT key (a dev CA) — SPKI won't match the KMS key.
	otherCA, err := devca.GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("other ca: %v", err)
	}
	if _, err := NewIssuer(ctx, f, "k", otherCA.CertPEM, testTD, time.Hour); err == nil {
		t.Fatal("expected NewIssuer to refuse a CA cert whose key is not the KMS key")
	}
}

// TestBootstrapCACert_Idempotent proves a matching existing cert is reused and a
// mismatched one is refused (never silently replaced).
func TestBootstrapCACert_Idempotent(t *testing.T) {
	f := newFakeKMS(t)
	signer, err := NewSigner(context.Background(), f, "k")
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	first, err := BootstrapCACert(signer, nil, time.Hour)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// Reuse: same key ⇒ returns the SAME bytes, no new cert minted.
	again, err := BootstrapCACert(signer, first, time.Hour)
	if err != nil {
		t.Fatalf("bootstrap idempotent: %v", err)
	}
	if string(again) != string(first) {
		t.Fatal("expected existing matching CA cert to be reused unchanged")
	}
	// Mismatch: a cert from another key must be refused.
	other, _ := devca.GenerateCA(time.Hour)
	if _, err := BootstrapCACert(signer, other.CertPEM, time.Hour); err == nil {
		t.Fatal("expected refusal of a mismatched existing CA cert")
	}
	// Append attack: a rogue cert APPENDED after the legitimate one must be
	// refused too — the SPKI check on the first cert alone would otherwise carry
	// the extra trust anchor forward on every idempotent re-run.
	appended := append(append([]byte{}, first...), other.CertPEM...)
	if _, err := BootstrapCACert(signer, appended, time.Hour); err == nil {
		t.Fatal("expected refusal of a bundle with an appended extra CA cert")
	}
}

// TestBootstrapCACert_EmitsCertOnly proves the bootstrap output is public material
// only — no private-key PEM block (there is no exportable key).
func TestBootstrapCACert_EmitsCertOnly(t *testing.T) {
	signer, _ := NewSigner(context.Background(), newFakeKMS(t), "k")
	certPEM, err := BootstrapCACert(signer, nil, time.Hour)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if strings.Contains(string(certPEM), "PRIVATE KEY") {
		t.Fatal("bootstrap output must NOT contain private key material")
	}
	if !strings.Contains(string(certPEM), "CERTIFICATE") {
		t.Fatal("bootstrap output must be a CERTIFICATE")
	}
}

// TestNewSigner_RefusesNonSignVerifyKey proves a wrong-usage KMS key is refused.
func TestNewSigner_RefusesNonSignVerifyKey(t *testing.T) {
	f := newFakeKMS(t)
	f.usage = kmstypes.KeyUsageTypeEncryptDecrypt
	if _, err := NewSigner(context.Background(), f, "k"); err == nil {
		t.Fatal("expected NewSigner to refuse a non-SIGN_VERIFY key")
	}
}

// TestSigner_RejectsNonSHA256 proves the signer refuses a non-SHA-256 hash (it can
// only ever sign the ECDSA_SHA_256 digests x509 produces for this key).
func TestSigner_RejectsNonSHA256(t *testing.T) {
	signer, _ := NewSigner(context.Background(), newFakeKMS(t), "k")
	if _, err := signer.Sign(rand.Reader, make([]byte, 20), crypto.SHA1); err == nil {
		t.Fatal("expected Sign to reject a non-SHA-256 digest")
	}
}

// --- Phase-1 review-hardening tests -----------------------------------------

// makeCACert builds a CA certificate carrying pub as its SubjectPublicKeyInfo,
// signed by signKey, with the given validity — the raw material for crafting the
// negative cases NewIssuer must refuse.
func makeCACert(t *testing.T, pub crypto.PublicKey, signKey crypto.Signer, notBefore, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber:          bigOne(),
		Subject:               pkix.Name{CommonName: "spawnly-substrate-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, signKey)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func bigOne() *big.Int { return big.NewInt(1) }

// TestNewIssuer_RefusesExpiredCACert: a CA cert past its NotAfter is refused at
// startup, not silently used to issue leaves that only fail downstream at SPIRE.
func TestNewIssuer_RefusesExpiredCACert(t *testing.T) {
	f := newFakeKMS(t)
	signer, err := NewSigner(context.Background(), f, "k")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	past := time.Now().Add(-48 * time.Hour)
	expired := makeCACert(t, signer.Public(), signer, past, past.Add(time.Hour))
	if _, err := NewIssuer(context.Background(), f, "k", expired, testTD, time.Hour); err == nil {
		t.Fatal("NewIssuer accepted an expired CA cert, want refusal")
	}
}

// TestNewIssuer_RefusesForeignSignedCACert: a cert embedding the KMS public key as
// its SPKI (so the SPKI check passes) but signed by a DIFFERENT key must be refused
// by the self-signature check — SPKI equality alone is not proof of provenance.
func TestNewIssuer_RefusesForeignSignedCACert(t *testing.T) {
	f := newFakeKMS(t)
	signer, err := NewSigner(context.Background(), f, "k")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	foreign, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen foreign: %v", err)
	}
	now := time.Now()
	// SPKI = the KMS public key; signature = the foreign key.
	crafted := makeCACert(t, signer.Public(), foreign, now.Add(-time.Minute), now.Add(time.Hour))
	if _, err := NewIssuer(context.Background(), f, "k", crafted, testTD, time.Hour); err == nil {
		t.Fatal("NewIssuer accepted a foreign-signed CA cert with matching SPKI, want refusal")
	}
}

// TestNewIssuer_TrustBundleIsSingleCert: a trailing appended cert block in the CA
// file must NOT ride into the trust bundle — TrustBundlePEM returns exactly the one
// validated CA cert.
func TestNewIssuer_TrustBundleIsSingleCert(t *testing.T) {
	f := newFakeKMS(t)
	signer, err := NewSigner(context.Background(), f, "k")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	caPEM, err := BootstrapCACert(signer, nil, time.Hour)
	if err != nil {
		t.Fatalf("BootstrapCACert: %v", err)
	}
	// Append a second, unrelated self-signed cert.
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen other: %v", err)
	}
	now := time.Now()
	extra := makeCACert(t, &other.PublicKey, other, now.Add(-time.Minute), now.Add(time.Hour))
	multi := append(append([]byte{}, caPEM...), extra...)

	iss, err := NewIssuer(context.Background(), f, "k", multi, testTD, time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer (multi-block, first is the CA): %v", err)
	}
	n := 0
	rest := iss.TrustBundlePEM()
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		n++
	}
	if n != 1 {
		t.Fatalf("TrustBundlePEM has %d cert blocks, want exactly 1 (no trailing cert leaked)", n)
	}
}
