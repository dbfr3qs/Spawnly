package substrate

// LeafIssuer is the CA-agnostic issuance seam the connect flow
// (cmd/orchestrator substrateAPI.connect) depends on. It is exactly the surface
// connect uses — no more — so the concrete CA can be swapped WITHOUT the connect
// flow knowing which CA is live:
//
//   - the LOCAL-SIM dev CA (*Issuer, backed by internal/substrate/devca), and
//   - the production edge KMS CA (internal/substrate/kmsca), whose private key
//     never leaves AWS KMS.
//
// Dependency direction: this interface stays AWS-blind. The KMS implementation is
// an adapter package that imports the AWS SDK and satisfies this seam; it is wired
// only in cmd/orchestrator. Nothing here (nor internal/substrate/entry.go) imports
// an AWS SDK — the neutral contract must not depend on the adapter.
//
// A nil LeafIssuer means the feature is not configured; callers MUST treat that as
// "substrate connect unavailable" (503), never as an open door.
type LeafIssuer interface {
	// IssueOrgIdentity mints a one-time org-bound leaf. orgID is normalized+
	// validated through the single org-id source (devca.NormalizeOrgID) so the org
	// stamped into the cert CommonName / SPIFFE segments matches the lowercase
	// Keycloak slug and can never smuggle a path separator. The org binding IS the
	// CommonName the SPIRE x509pop template reads, so node→org is cryptographic.
	IssueOrgIdentity(orgID string) (IssuedIdentity, error)
	// OrgID reads the normalized org a leaf is bound to (its CommonName), for the
	// connect flow to assert the issued identity matches the requested org.
	OrgID(leafCertPEM []byte) (string, error)
	// TrustBundlePEM returns the CA CERTIFICATE (never any private key) — the
	// bootstrap trust bundle the substrate SPIRE agent uses to verify the hosted
	// server's x509pop challenge. Safe to hand to the org admin alongside the leaf.
	TrustBundlePEM() []byte
}

// Compile-time proof the dev-CA Issuer satisfies the seam unchanged — the local
// path keeps its exact behavior while the connect flow depends only on LeafIssuer.
var _ LeafIssuer = (*Issuer)(nil)
