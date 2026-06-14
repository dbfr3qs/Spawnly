// Package controlplane authenticates the platform's own backend services (the
// orchestrator and the IdP's CIBA driver) when they call the registry's
// consent-lifecycle endpoints.
//
// It is deliberately separate from internal/registrant: registrant authenticates
// an *agent* registering itself and derives that agent's id, whereas a
// control-plane caller is a platform service acting on behalf of the system, not
// any single agent — so it proves a *service* identity (a shared secret or a
// client-credentials access token), not an agent SVID.
//
// Per the platform dependency-direction rule, no implementation depends on a
// specific IdP. The OIDC authenticator validates tokens against a *configured*
// JWKS (IdentityServer, Cognito, Auth0, ...); it never calls a particular IdP.
package controlplane

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
)

// Caller is the authenticated control-plane service. ClientID identifies which
// service called (for audit and, later, per-caller authorization); Scopes are
// the scopes it presented. Both are empty for the open (AllowAll) authenticator.
type Caller struct {
	ClientID string
	Scopes   []string
}

// Authenticator verifies that an inbound request is from an authorized
// control-plane caller. It must not write to the response — the caller (the
// registry's consent handlers) owns the HTTP response and turns an error into
// 401 Unauthorized.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Caller, error)
}

// AllowAll authorizes every request. It is the default for local/demo
// deployments and tests, where the consent endpoints run open behind network
// isolation (CONTROL_PLANE_AUTH unset/"none").
func AllowAll() Authenticator { return allowAll{} }

type allowAll struct{}

func (allowAll) Authenticate(context.Context, *http.Request) (Caller, error) {
	return Caller{ClientID: "anonymous"}, nil
}

// NewSharedSecret returns an Authenticator that requires every caller to present
// "Authorization: Bearer <token>". It is the zero-infra tier: one secret shared
// by the registry and its trusted callers, compared in constant time.
func NewSharedSecret(token string) Authenticator {
	return &sharedSecret{want: []byte("Bearer " + token)}
}

type sharedSecret struct{ want []byte }

func (s *sharedSecret) Authenticate(_ context.Context, r *http.Request) (Caller, error) {
	got := []byte(r.Header.Get("Authorization"))
	if subtle.ConstantTimeCompare(got, s.want) != 1 {
		return Caller{}, errors.New("invalid control-plane token")
	}
	return Caller{ClientID: "shared-secret"}, nil
}
