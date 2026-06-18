// Package attestor defines the neutral "how does a workload prove who it is"
// contract used by the agent sidecar. It is the workload-side half of the
// platform's pluggable attestation: implementations (SPIFFE/SPIRE, AWS IRSA,
// ...) adapt a specific attestor to the Source below, but this file itself
// depends on nothing credential-shape specific.
//
// The control-plane half — verifying the presented credential and deriving an
// agent identity from it — lives in internal/registrant (registry side) and
// identityserver (token-minting side).
package attestor

import "context"

// JWTBearerAssertionType is the OAuth client-assertion type for a signed JWT.
// Both SPIFFE JWT-SVIDs and IRSA-projected ServiceAccount tokens are JWTs and
// are presented at the token endpoint under this type.
const JWTBearerAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"

// Credential is an attestation credential a workload presents to the control
// plane to prove its identity.
type Credential struct {
	// Value is the assertion itself — a marshaled JWT-SVID under SPIFFE/SPIRE,
	// a projected ServiceAccount token under AWS IRSA.
	Value string
	// AssertionType is the OAuth client-assertion-type URN under which Value is
	// presented at the token endpoint (e.g. JWTBearerAssertionType).
	AssertionType string
}

// Source fetches a fresh attestation credential for the given token audience.
// Implementations adapt a specific attestor (the SPIRE workload API, AWS IRSA,
// ...) to this neutral contract.
type Source interface {
	Fetch(ctx context.Context, audience string) (Credential, error)
}

// MockSource is a Source for tests: it returns Cred (or Err) for every Fetch.
type MockSource struct {
	Cred Credential
	Err  error
}

// Fetch implements Source.
func (m *MockSource) Fetch(context.Context, string) (Credential, error) {
	return m.Cred, m.Err
}
