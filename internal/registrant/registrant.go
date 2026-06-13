// Package registrant defines the pluggable "who is allowed to register an
// agent, and what agentID do they get" abstraction used by the registry's
// POST /v1/agents handler. It is the neutral contract: implementations
// (SPIFFE, OIDC, mTLS, ...) adapt a specific credential format to the
// Identity below, but this file itself depends on nothing credential-shape
// specific.
package registrant

import (
	"context"
	"net/http"
)

// Identity is the verified caller, decoupled from the credential format used
// to prove it.
type Identity struct {
	// AgentID is the registry-facing identifier for the agent being
	// registered — the primary key in registry.AgentRecord. Generalizes
	// today's path.Base(spiffeID). Must be non-empty on success.
	AgentID string

	// Subject is the raw verified identity string from the credential
	// (SPIFFE ID, OIDC "sub", or cert SAN) — kept for logging/auditing.
	Subject string

	// Issuer identifies which verifier produced this identity
	// ("spiffe-svid", "oidc", "mtls") — useful for audit logs and for
	// deployments that may one day mix verifier types.
	Issuer string
}

// Verifier authenticates an inbound registration request and derives the
// agent's identity. Implementations may read the Authorization header, the
// TLS peer certificate (r.TLS.PeerCertificates), or both.
//
// Verify must not write to the http.ResponseWriter — the caller (the
// registry's POST /v1/agents handler) owns the HTTP response and translates
// a returned error into 401 Unauthorized. On success, AgentID must be
// non-empty.
type Verifier interface {
	Verify(ctx context.Context, r *http.Request) (Identity, error)
}
