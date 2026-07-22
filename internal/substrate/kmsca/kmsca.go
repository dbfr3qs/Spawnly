// Package kmsca is the PRODUCTION-EDGE certificate authority for x509pop node
// attestation: it issues the same org-bound x509 leaves the dev CA does, but the
// CA private key lives in AWS KMS and NEVER leaves it. Every signature is a
// `kms:Sign` call; the CA public key is fetched once via `kms:GetPublicKey` and
// cached. There is no exportable private key here — only public material (the CA
// CERTIFICATE and the issued leaves) is ever produced.
//
// # Why this exists / the dev-CA relationship
//
// The dev CA (internal/substrate/devca) is LOCAL-SIM only and must never ship to
// AWS. On the real edge the org-bound leaves must be signed by an AWS KMS
// asymmetric key. Both CAs share ONE leaf-construction core (devca.BuildOrgLeaf)
// and ONE record-derivation funnel (substrate.NewIssuedIdentity), so the org
// binding (CN=org + org-scoped SAN) and the x509pop node→org derivation are
// byte-identical across the two — swapping CAs changes WHO signs, never the
// attestation semantics.
//
// # Dependency direction
//
// This is an ADAPTER: it imports the AWS SDK and satisfies the AWS-blind
// substrate.LeafIssuer seam. It is wired only in cmd/orchestrator. The neutral
// contract (internal/substrate) never imports this package or the AWS SDK.
package kmsca

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"
	kmstypes "github.com/aws/aws-sdk-go-v2/service/kms/types"

	"github.com/spawnly/platform/internal/substrate"
	"github.com/spawnly/platform/internal/substrate/devca"
)

// SigningAPI is the NARROW slice of the AWS KMS API this package uses — exactly
// the two least-privilege actions the issuer needs (`kms:Sign` per signature,
// `kms:GetPublicKey` once at startup). Keeping it an interface lets unit tests
// substitute an in-process crypto.Signer-backed fake — no real AWS in tests — and
// documents that the issuer can do nothing else with the key (no decrypt, no
// export, no key management).
type SigningAPI interface {
	Sign(ctx context.Context, in *kms.SignInput, optFns ...func(*kms.Options)) (*kms.SignOutput, error)
	GetPublicKey(ctx context.Context, in *kms.GetPublicKeyInput, optFns ...func(*kms.Options)) (*kms.GetPublicKeyOutput, error)
}

// NewSigningClient builds a real AWS KMS client from the default credential chain
// (EKS Pod Identity on the edge), mirroring the stsweb attestor's construction.
func NewSigningClient(ctx context.Context) (SigningAPI, error) {
	cfg, err := loadConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}
	return kms.NewFromConfig(cfg), nil
}

// Signer adapts an AWS KMS asymmetric signing key to crypto.Signer so the shared
// devca.BuildOrgLeaf / devca.BuildSelfSignedCACert cores can sign through it
// exactly as they sign through an in-process RSA key. The key is ECDSA P-256
// (ECDSA_SHA_256); the public key is cached at construction. Sign performs one
// `kms:Sign` (MessageType=DIGEST) per call — the private key never leaves KMS.
type Signer struct {
	api   SigningAPI
	keyID string
	pub   *ecdsa.PublicKey
}

// NewSigner fetches and caches the KMS key's public half and verifies it is an
// ECDSA P-256 key usable for ECDSA_SHA_256 signing. Fails closed on any other key
// spec so the issuer can never be pointed at a key whose signatures x509 can't
// chain. keyID may be a key id, ARN, or alias.
func NewSigner(ctx context.Context, api SigningAPI, keyID string) (*Signer, error) {
	if api == nil {
		return nil, errors.New("nil KMS API")
	}
	if keyID == "" {
		return nil, errors.New("empty KMS key id")
	}
	out, err := api.GetPublicKey(ctx, &kms.GetPublicKeyInput{KeyId: aws.String(keyID)})
	if err != nil {
		return nil, fmt.Errorf("kms GetPublicKey: %w", err)
	}
	if out.KeyUsage != kmstypes.KeyUsageTypeSignVerify {
		return nil, fmt.Errorf("kms key usage %q is not SIGN_VERIFY", out.KeyUsage)
	}
	pubAny, err := x509.ParsePKIXPublicKey(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("parse kms public key: %w", err)
	}
	pub, ok := pubAny.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("kms key is %T, want ECDSA P-256", pubAny)
	}
	if pub.Curve != elliptic.P256() {
		return nil, fmt.Errorf("kms key curve %s is not P-256", pub.Curve.Params().Name)
	}
	// Require the key actually advertises ECDSA_SHA_256 — belt-and-suspenders over
	// the curve check, so a misconfigured key spec is refused at startup.
	if !hasSigningAlgorithm(out.SigningAlgorithms, kmstypes.SigningAlgorithmSpecEcdsaSha256) {
		return nil, fmt.Errorf("kms key does not support ECDSA_SHA_256")
	}
	return &Signer{api: api, keyID: keyID, pub: pub}, nil
}

func hasSigningAlgorithm(algs []kmstypes.SigningAlgorithmSpec, want kmstypes.SigningAlgorithmSpec) bool {
	for _, a := range algs {
		if a == want {
			return true
		}
	}
	return false
}

// Public returns the cached CA public key (ECDSA P-256).
func (s *Signer) Public() crypto.PublicKey { return s.pub }

// Sign implements crypto.Signer over `kms:Sign`. x509.CreateCertificate pre-hashes
// the TBS with SHA-256 and passes the 32-byte digest here; we forward it to KMS
// with MessageType=DIGEST + ECDSA_SHA_256. KMS returns an ASN.1 DER ECDSA
// signature — exactly what crypto/x509 expects — so no re-encoding is needed. The
// rand argument is ignored: KMS supplies the nonce inside the HSM.
func (s *Signer) Sign(_ io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	if opts == nil || opts.HashFunc() != crypto.SHA256 {
		return nil, fmt.Errorf("kmsca signer requires SHA-256, got %v", opts)
	}
	if len(digest) != crypto.SHA256.Size() {
		return nil, fmt.Errorf("digest length %d != SHA-256 size", len(digest))
	}
	// crypto.Signer.Sign cannot carry a context; the KMS client's own
	// retry/timeout config bounds the call.
	out, err := s.api.Sign(context.Background(), &kms.SignInput{
		KeyId:            aws.String(s.keyID),
		Message:          digest,
		MessageType:      kmstypes.MessageTypeDigest,
		SigningAlgorithm: kmstypes.SigningAlgorithmSpecEcdsaSha256,
	})
	if err != nil {
		return nil, fmt.Errorf("kms Sign: %w", err)
	}
	if len(out.Signature) == 0 {
		return nil, errors.New("kms Sign returned empty signature")
	}
	return out.Signature, nil
}

// DefaultCASubject is the self-signed CA certificate subject the bootstrap emits.
var DefaultCASubject = pkix.Name{CommonName: "spawnly-substrate-ca", Organization: []string{"spawnly-substrate"}}

// DefaultCATTL is the CA certificate validity when none is given (10 years — the
// CA cert is long-lived public material; the KMS key itself has no expiry).
const DefaultCATTL = 10 * 365 * 24 * time.Hour

// BootstrapCACert produces the self-signed CA CERTIFICATE (PEM) for the KMS key —
// PUBLIC material only, signed via `kms:Sign`. It is idempotent: if existingPEM is
// a CA cert whose SubjectPublicKeyInfo matches the KMS public key, it is returned
// unchanged (so re-running the bootstrap against a stored ConfigMap is a no-op);
// otherwise a fresh cert is minted. A non-empty existingPEM whose SPKI does NOT
// match the KMS key is a hard error — never silently replaced — so a mismatched CA
// cert can't be quietly swapped in. ttl<=0 uses DefaultCATTL.
func BootstrapCACert(signer *Signer, existingPEM []byte, ttl time.Duration) ([]byte, error) {
	if signer == nil {
		return nil, errors.New("nil signer")
	}
	if len(bytes.TrimSpace(existingPEM)) > 0 {
		match, err := certMatchesSigner(existingPEM, signer)
		if err != nil {
			return nil, fmt.Errorf("inspect existing CA cert: %w", err)
		}
		if match {
			return existingPEM, nil
		}
		return nil, errors.New("existing CA cert does not match the KMS key (refusing to replace)")
	}
	if ttl <= 0 {
		ttl = DefaultCATTL
	}
	return devca.BuildSelfSignedCACert(signer, DefaultCASubject, ttl)
}

// certMatchesSigner reports whether certPEM's public key equals the signer's
// public key (SPKI byte-equality). This is the SINGLE trust check binding a CA
// cert to the KMS key — the leaf-signing path relies on it so a leaf never chains
// to a cert whose key isn't the one KMS actually signs with.
func certMatchesSigner(certPEM []byte, signer *Signer) (bool, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return false, err
	}
	certSPKI, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return false, err
	}
	signerSPKI, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return false, err
	}
	return bytes.Equal(certSPKI, signerSPKI), nil
}

// Issuer is the KMS-backed substrate.LeafIssuer. It composes the KMS Signer with
// the shared devca.BuildOrgLeaf core and the shared substrate.NewIssuedIdentity
// funnel, and holds the CA CERTIFICATE (public) it hands out as the trust bundle.
type Issuer struct {
	signer      *Signer
	caCert      *x509.Certificate
	caCertPEM   []byte
	trustDomain string
	leafTTL     time.Duration
}

// Compile-time proof the KMS issuer satisfies the same seam the connect flow uses.
var _ substrate.LeafIssuer = (*Issuer)(nil)

// NewIssuer builds the KMS issuer. It fetches+validates the KMS public key, loads
// the provided CA CERTIFICATE (cert-only — there is no key file), and REFUSES
// unless the CA cert's SPKI matches the KMS key (a wrong-key / mismatched CA cert
// can never issue). Fails closed on empty trust domain. This is the security spine:
// the issuer only ever signs leaves that chain to a CA whose key is the KMS key.
func NewIssuer(ctx context.Context, api SigningAPI, keyID string, caCertPEM []byte, trustDomain string, leafTTL time.Duration) (*Issuer, error) {
	if trustDomain == "" {
		return nil, errors.New("empty trust domain")
	}
	if len(bytes.TrimSpace(caCertPEM)) == 0 {
		return nil, errors.New("empty CA certificate PEM")
	}
	signer, err := NewSigner(ctx, api, keyID)
	if err != nil {
		return nil, err
	}
	caCert, err := parseCert(caCertPEM)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !caCert.IsCA {
		return nil, errors.New("provided certificate is not a CA certificate")
	}
	match, err := certMatchesSigner(caCertPEM, signer)
	if err != nil {
		return nil, fmt.Errorf("verify CA cert against KMS key: %w", err)
	}
	if !match {
		return nil, errors.New("CA certificate public key does not match the KMS key")
	}
	if leafTTL <= 0 {
		leafTTL = 90 * 24 * time.Hour
	}
	return &Issuer{
		signer:      signer,
		caCert:      caCert,
		caCertPEM:   caCertPEM,
		trustDomain: trustDomain,
		leafTTL:     leafTTL,
	}, nil
}

// IssueOrgIdentity mints an org-bound leaf signed by the KMS key, through the exact
// same builder + record-derivation funnel as the dev CA. orgID is normalized via
// the single org-id source, so the org in the CN / SPIFFE segments matches the
// lowercase Keycloak slug and can never smuggle a path separator.
func (i *Issuer) IssueOrgIdentity(orgID string) (substrate.IssuedIdentity, error) {
	normOrg, err := devca.NormalizeOrgID(orgID)
	if err != nil {
		return substrate.IssuedIdentity{}, fmt.Errorf("invalid org id: %w", err)
	}
	leaf, err := devca.BuildOrgLeaf(i.signer, i.caCert, i.trustDomain, normOrg, i.leafTTL)
	if err != nil {
		return substrate.IssuedIdentity{}, fmt.Errorf("issue org leaf: %w", err)
	}
	return substrate.NewIssuedIdentity(i.trustDomain, leaf)
}

// OrgID exposes the normalized org a leaf is bound to (its CommonName).
func (i *Issuer) OrgID(leafCertPEM []byte) (string, error) {
	return devca.OrgFromLeaf(leafCertPEM)
}

// TrustBundlePEM returns the CA CERTIFICATE (never any key) — the bootstrap trust
// bundle handed to the org admin alongside the leaf.
func (i *Issuer) TrustBundlePEM() []byte { return i.caCertPEM }
