package controlplane

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"golang.org/x/oauth2/clientcredentials"
)

// BearerSource returns a function yielding the control-plane bearer a trusted
// platform service (the orchestrator, the operator) presents to the registry.
// It is the client-side counterpart to the server-side Authenticator in this
// package, selected by the SAME CONTROL_PLANE_AUTH env var so the two ends agree:
//
//	none/unset    -> "" (no header; the registry's AllowAll authenticator runs open)
//	shared-secret -> the static CONTROL_PLANE_TOKEN
//	oidc          -> a client-credentials access token, fetched and auto-refreshed
//	                 by the oauth2 TokenSource
//
// The returned function is safe to call per request: for oidc it returns the
// current (refreshed) token, and on a refresh error it returns "" — the caller's
// bearerTransport then sends no Authorization header, so the registry fails the
// request closed rather than accepting a stale token. A misconfiguration (missing
// CONTROL_PLANE_TOKEN, or CLIENT_ID/TOKEN_URL for oidc) returns an error; an
// unknown CONTROL_PLANE_AUTH value likewise errors.
func BearerSource(ctx context.Context) (func() string, error) {
	switch v := os.Getenv("CONTROL_PLANE_AUTH"); v {
	case "", "none":
		return func() string { return "" }, nil
	case "shared-secret":
		token := os.Getenv("CONTROL_PLANE_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("CONTROL_PLANE_TOKEN required when CONTROL_PLANE_AUTH=shared-secret")
		}
		return func() string { return token }, nil
	case "oidc":
		scope := os.Getenv("CONTROL_PLANE_SCOPE")
		if scope == "" {
			scope = "registry.consent"
		}
		cfg := clientcredentials.Config{
			ClientID:     os.Getenv("CONTROL_PLANE_CLIENT_ID"),
			ClientSecret: os.Getenv("CONTROL_PLANE_CLIENT_SECRET"),
			TokenURL:     os.Getenv("CONTROL_PLANE_TOKEN_URL"),
			Scopes:       strings.Fields(scope),
		}
		if cfg.ClientID == "" || cfg.TokenURL == "" {
			return nil, fmt.Errorf("CONTROL_PLANE_CLIENT_ID and CONTROL_PLANE_TOKEN_URL required when CONTROL_PLANE_AUTH=oidc")
		}
		ts := cfg.TokenSource(ctx)
		return func() string {
			tok, err := ts.Token()
			if err != nil {
				log.Printf("control-plane token fetch failed: %v", err)
				return ""
			}
			return tok.AccessToken
		}, nil
	default:
		return nil, fmt.Errorf("unknown CONTROL_PLANE_AUTH %q", v)
	}
}
