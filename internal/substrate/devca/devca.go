// Package devca is the LOCAL-SIM dev certificate authority that backs x509pop
// node attestation for the remote substrate.
//
// # What this is NOT
//
// This is a THROWAWAY CA for the local kind simulation and the e2e harness. It
// is NOT the real Spawnly CA — that is edge/infra track material managed like
// the platform's other secrets (see spec.md "The hosted SPIRE server is
// edge-exposed"). No CA or key material generated here is ever committed to the
// repo or baked into an image; scripts write it into cluster Secrets at
// bootstrap/setup and gitignore any local copy.
//
// # The org binding
//
// The control plane (here: scripts/substrate-dev-cert.sh, in Phase 3 the connect
// API) ISSUES an org-bound leaf off this CA: the org id is stamped into the
// leaf's Subject CommonName and a matching org-scoped URI SAN. Because the leaf
// is CP-signed and the SPIRE server trusts ONLY this CA, a connecting node cannot
// forge or alter its org — admin auth + CP issuance is the binding (spec.md
// Behaviour §1). The SPIRE x509pop server plugin's agent_path_template
// (NodeAttestorPathTemplate) reads the CommonName to place the node under
// spiffe://<td>/spire/agent/x509pop/org/{orgID}/{fingerprint}, so node→org is
// cryptographic, not a lookup.
//
// # Why org ids are validated
//
// The org id becomes a path segment in BOTH the node SPIFFE ID (via the server
// template) and the workload SVID (spiffe.BuildOrgAgentPath). If a caller could
// smuggle a '/' or template metacharacters into it, they could reshape the node
// path and claim another org's namespace. ValidateOrgID enforces a strict
// charset so that is impossible regardless of the template engine.
package devca

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"time"
)

// NodeAttestorPathTemplate is the SPIRE x509pop server plugin's
// agent_path_template. It embeds the org from the leaf's CommonName so the
// attested node's SPIFFE ID carries the org cryptographically:
//
//	spiffe://<td>/spire/agent/x509pop/org/{CN}/{fingerprint}
//
// scripts/substrate-spire-x509pop.sh injects this verbatim into the server
// config, and NodeSpiffePath computes the resulting id for entry parenting.
const NodeAttestorPathTemplate = "/spire/agent/x509pop/org/{{ .Subject.CommonName }}/{{ .Fingerprint }}"

// NodeSpiffePathPrefix returns the org-scoped prefix of the node SPIFFE ID the
// x509pop server assigns via NodeAttestorPathTemplate, up to (but excluding) the
// per-cert fingerprint:
//
//	spiffe://<td>/spire/agent/x509pop/org/{orgID}/
//
// The entry creator resolves the exact node id (prefix + fingerprint) from
// `spire-server agent list`; this prefix is what it matches on to confirm the
// node landed under the right org.
func NodeSpiffePathPrefix(trustDomain, orgID string) (string, error) {
	if trustDomain == "" {
		return "", errors.New("empty trust domain")
	}
	if err := ValidateOrgID(orgID); err != nil {
		return "", err
	}
	return "spiffe://" + trustDomain + "/spire/agent/x509pop/org/" + orgID + "/", nil
}

// orgIDPattern is the strict charset for an org id used as a path segment. No
// '/', no template metacharacters, no whitespace — a DNS-label-ish token.
var orgIDPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?$`)

// ValidateOrgID fails closed on any org id that is empty or contains anything
// outside the strict charset, so it can never reshape a SPIFFE path.
func ValidateOrgID(orgID string) error {
	if orgID == "" {
		return errors.New("empty org id")
	}
	if !orgIDPattern.MatchString(orgID) {
		return fmt.Errorf("org id %q not a valid path token ([A-Za-z0-9._-], no leading/trailing punctuation)", orgID)
	}
	return nil
}

// CA is a parsed dev CA: the signing certificate and its private key. Keep the
// key in-process only — persist it (if at all) into a cluster Secret, never a
// file the server mounts (the server trusts the CA CERT, and never needs the
// key).
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	Key     *rsa.PrivateKey
}

// GenerateCA mints a fresh self-signed dev CA. ttl bounds its validity; callers
// use a short-lived CA for the sim.
func GenerateCA(ttl time.Duration) (*CA, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate ca key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "spawnly-substrate-dev-ca", Organization: []string{"spawnly-substrate-dev"}},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(ttl),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign ca: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parse ca: %w", err)
	}
	return &CA{
		Cert:    cert,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:     key,
	}, nil
}

// LoadCA parses a CA from PEM-encoded cert + key (as stored in the cluster
// Secret and read back by the dev-cert issuer).
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("load ca cert: %w", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("load ca key: no PEM block")
	}
	key, err := parsePrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("load ca key: %w", err)
	}
	return &CA{Cert: cert, CertPEM: certPEM, Key: key}, nil
}

// KeyPEM returns the CA private key in PKCS#8 PEM (for writing into the Secret
// the issuer reads — never a file the server mounts).
func (c *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.Key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// Leaf is an issued org-bound identity: the PEM cert + key an org's substrate
// SPIRE agent presents for x509pop attestation.
type Leaf struct {
	CertPEM []byte
	KeyPEM  []byte
}

// IssueOrgLeaf signs an org-bound leaf off the CA for the given trust domain +
// org id. The org is stamped into the CommonName (which the server template
// reads) AND a matching org-scoped URI SAN, so the binding is explicit and
// self-describing. Fails closed on an invalid org id.
func IssueOrgLeaf(ca *CA, trustDomain, orgID string, ttl time.Duration) (*Leaf, error) {
	if err := ValidateOrgID(orgID); err != nil {
		return nil, err
	}
	if trustDomain == "" {
		return nil, errors.New("empty trust domain")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}
	// A documentary org-scoped SAN. The path template binds on CommonName; the
	// SAN makes the intended org unambiguous to a human reading the cert.
	sanURI := &url.URL{Scheme: "spiffe", Host: trustDomain, Path: "/org/" + orgID + "/substrate"}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: orgID, Organization: []string{"spawnly-substrate"}},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{sanURI},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("sign leaf: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return &Leaf{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// OrgFromLeaf reads the org id (CommonName) off a leaf cert PEM — the same field
// the server template binds on, so tests can assert the two agree.
func OrgFromLeaf(certPEM []byte) (string, error) {
	cert, err := parseCert(certPEM)
	if err != nil {
		return "", err
	}
	if err := ValidateOrgID(cert.Subject.CommonName); err != nil {
		return "", fmt.Errorf("leaf CommonName is not a valid org id: %w", err)
	}
	return cert.Subject.CommonName, nil
}

// VerifyLeaf returns nil iff leaf chains to ca. This is the exact check the
// SPIRE x509pop server performs (external_pki mode, ca_bundle_path == this CA):
// a leaf signed by a different/forged CA fails, which is AC 1's negative.
func VerifyLeaf(ca *CA, leafCertPEM []byte) error {
	leaf, err := parseCert(leafCertPEM)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	return err
}

func randSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("serial: %w", err)
	}
	return serial, nil
}

func parseCert(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(b.Bytes)
}

func parsePrivateKey(der []byte) (*rsa.PrivateKey, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("not an RSA key")
	}
	return x509.ParsePKCS1PrivateKey(der)
}
