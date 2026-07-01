package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBearerSource covers the control-plane bearer source selected by
// CONTROL_PLANE_AUTH: the none/unset tier yields no token, shared-secret yields
// the static token (and errors when it is missing), and oidc requires its client
// config. This is the client-side counterpart the orchestrator and operator both
// use, so its tiers must match the server-side authenticator's.
func TestBearerSource(t *testing.T) {
	t.Run("unset yields empty token, no error", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "")
		src, err := BearerSource(context.Background())
		if err != nil {
			t.Fatalf("unset: unexpected error %v", err)
		}
		if got := src(); got != "" {
			t.Fatalf("unset token = %q, want empty", got)
		}
	})

	t.Run("none yields empty token", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "none")
		src, err := BearerSource(context.Background())
		if err != nil {
			t.Fatalf("none: unexpected error %v", err)
		}
		if got := src(); got != "" {
			t.Fatalf("none token = %q, want empty", got)
		}
	})

	t.Run("shared-secret returns the static token", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "shared-secret")
		t.Setenv("CONTROL_PLANE_TOKEN", "sekrit")
		src, err := BearerSource(context.Background())
		if err != nil {
			t.Fatalf("shared-secret: unexpected error %v", err)
		}
		if got := src(); got != "sekrit" {
			t.Fatalf("shared-secret token = %q, want sekrit", got)
		}
	})

	t.Run("shared-secret without a token errors", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "shared-secret")
		t.Setenv("CONTROL_PLANE_TOKEN", "")
		if _, err := BearerSource(context.Background()); err == nil {
			t.Fatal("shared-secret without CONTROL_PLANE_TOKEN: want error, got nil")
		}
	})

	t.Run("oidc without client config errors", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "oidc")
		t.Setenv("CONTROL_PLANE_CLIENT_ID", "")
		t.Setenv("CONTROL_PLANE_TOKEN_URL", "")
		if _, err := BearerSource(context.Background()); err == nil {
			t.Fatal("oidc without client id/token url: want error, got nil")
		}
	})

	t.Run("oidc with client config builds a source", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "oidc")
		t.Setenv("CONTROL_PLANE_CLIENT_ID", "orchestrator")
		t.Setenv("CONTROL_PLANE_CLIENT_SECRET", "shh")
		t.Setenv("CONTROL_PLANE_TOKEN_URL", "http://idp.invalid/token")
		src, err := BearerSource(context.Background())
		if err != nil {
			t.Fatalf("oidc: unexpected error %v", err)
		}
		if src == nil {
			t.Fatal("oidc: source func is nil")
		}
		// Not invoked: calling src() would attempt a live token fetch. Building it
		// without error (given the required config) is the contract under test.
	})

	t.Run("oidc refresh failure fails closed (empty token, not stale)", func(t *testing.T) {
		// A token endpoint that always errors — the source must yield "" so the
		// caller sends no Authorization header (registry 401s) rather than a
		// stale/blank bearer.
		idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "unavailable", http.StatusInternalServerError)
		}))
		defer idp.Close()
		t.Setenv("CONTROL_PLANE_AUTH", "oidc")
		t.Setenv("CONTROL_PLANE_CLIENT_ID", "orchestrator")
		t.Setenv("CONTROL_PLANE_CLIENT_SECRET", "shh")
		t.Setenv("CONTROL_PLANE_TOKEN_URL", idp.URL+"/token")
		src, err := BearerSource(context.Background())
		if err != nil {
			t.Fatalf("oidc build: unexpected error %v", err)
		}
		if got := src(); got != "" {
			t.Fatalf("oidc token on refresh failure = %q, want empty (fail-closed)", got)
		}
	})

	t.Run("unknown auth errors", func(t *testing.T) {
		t.Setenv("CONTROL_PLANE_AUTH", "bogus")
		if _, err := BearerSource(context.Background()); err == nil {
			t.Fatal("unknown CONTROL_PLANE_AUTH: want error, got nil")
		}
	})
}
