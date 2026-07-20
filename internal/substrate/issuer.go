package substrate

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/spawnly/platform/internal/substrate/devca"
)

// Issuer issues an org's bring-your-own-compute x509 identity off the dev CA
// (remote-substrate Phase 3). The connect flow calls it ONCE per connect/rotate:
// the returned leaf (cert + key) is handed to the org admin exactly once and
// NEVER persisted server-side — only the fingerprint/serial it also returns are
// kept on the substrate record. Admin authentication + this CP issuance IS the
// org binding, so there is no proof-of-control step.
//
// The dev CA private key lives ONLY in this in-process Issuer (loaded from a
// cluster Secret at startup) — never written to a file the SPIRE server mounts
// (the server trusts the CA CERT only). A nil Issuer means the feature is not
// configured; callers must treat that as "substrate connect unavailable", not as
// an open door.
type Issuer struct {
	ca          *devca.CA
	trustDomain string
	leafTTL     time.Duration
}

// NewIssuer builds an Issuer from a loaded dev CA, the deployment trust domain,
// and the leaf validity. It fails closed on a nil CA / empty trust domain so a
// misconfigured deployment can never issue an unbound or wrong-domain identity.
func NewIssuer(ca *devca.CA, trustDomain string, leafTTL time.Duration) (*Issuer, error) {
	if ca == nil {
		return nil, errors.New("nil dev CA")
	}
	if trustDomain == "" {
		return nil, errors.New("empty trust domain")
	}
	if leafTTL <= 0 {
		leafTTL = 90 * 24 * time.Hour
	}
	return &Issuer{ca: ca, trustDomain: trustDomain, leafTTL: leafTTL}, nil
}

// IssuedIdentity is the result of an issuance: the one-time leaf material plus the
// fingerprint/serial the caller persists on the substrate record and the x509pop
// NODE id the org's SPIRE agent will attest to (so the connect flow can parent the
// substrate-agent bootstrap entry to it BEFORE the node has attested).
type IssuedIdentity struct {
	Leaf            *devca.Leaf
	CertFingerprint string // SHA-256 hex of the leaf DER (retained on the record)
	CertSerial      string // leaf serial number, hex
	// NodeID is the SPIFFE node id the SPIRE x509pop server will assign this leaf:
	// spiffe://<td>/spire/agent/x509pop/org/{org}/{sha1-hex-of-DER}. The x509pop
	// plugin fingerprints the leaf as SHA-1 of its DER (matching the Phase-2
	// scripts/substrate-verify.sh derivation), which the connect flow needs to
	// pre-author the substrate-agent's bootstrap entry.
	NodeID string
	// ExpiresAt is the issued leaf's actual NotAfter — the true expiry to report,
	// rather than recomputing now+TTL (which would skew and could diverge from the
	// leaf if the TTLs ever differ).
	ExpiresAt time.Time
}

// IssueOrgIdentity mints an org-bound leaf. orgID is normalized+validated through
// the single org-id source (devca.NormalizeOrgID) so the org that ends up in the
// cert CommonName / SPIFFE segments matches the lowercase Keycloak slug and can
// never smuggle a '/' into a path. The org binding is the CommonName the SPIRE
// x509pop server template reads — see devca.IssueOrgLeaf.
func (i *Issuer) IssueOrgIdentity(orgID string) (IssuedIdentity, error) {
	normOrg, err := devca.NormalizeOrgID(orgID)
	if err != nil {
		return IssuedIdentity{}, fmt.Errorf("invalid org id: %w", err)
	}
	leaf, err := devca.IssueOrgLeaf(i.ca, i.trustDomain, normOrg, i.leafTTL)
	if err != nil {
		return IssuedIdentity{}, fmt.Errorf("issue org leaf: %w", err)
	}
	cert, err := parseLeaf(leaf.CertPEM)
	if err != nil {
		return IssuedIdentity{}, err
	}
	sum := sha256.Sum256(cert.Raw)
	// NodeID via the shared devca helper — the SINGLE source of the x509pop
	// SHA-1 node-fingerprint decision, so the connect flow and the substrate-agent
	// derive the SAME parent id for org entries.
	nodeID, err := devca.NodeIDFromLeaf(i.trustDomain, leaf.CertPEM)
	if err != nil {
		return IssuedIdentity{}, fmt.Errorf("node id: %w", err)
	}
	return IssuedIdentity{
		Leaf:            leaf,
		CertFingerprint: hex.EncodeToString(sum[:]),
		CertSerial:      cert.SerialNumber.Text(16),
		NodeID:          nodeID,
		ExpiresAt:       cert.NotAfter,
	}, nil
}

// OrgID exposes the normalized org a leaf is bound to (its CommonName), for the
// connect flow to assert the issued identity matches the requested org.
func (i *Issuer) OrgID(leafCertPEM []byte) (string, error) {
	return devca.OrgFromLeaf(leafCertPEM)
}

// TrustBundlePEM returns the dev CA CERT (never the key) — the bootstrap trust
// bundle the substrate SPIRE agent uses to verify the hosted server's x509pop
// challenge response. Safe to hand to the admin alongside the leaf.
func (i *Issuer) TrustBundlePEM() []byte {
	return i.ca.CertPEM
}

// parseLeaf decodes a PEM leaf to an x509 certificate.
func parseLeaf(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, errors.New("issued leaf has no CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse issued leaf: %w", err)
	}
	return cert, nil
}
