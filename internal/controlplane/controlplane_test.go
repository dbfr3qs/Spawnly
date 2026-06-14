package controlplane

import (
	"context"
	"net/http"
	"testing"
)

func req(authHeader string) *http.Request {
	r, _ := http.NewRequest("POST", "/v1/consent-requests/x/approve", nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	return r
}

func TestAllowAll(t *testing.T) {
	c, err := AllowAll().Authenticate(context.Background(), req(""))
	if err != nil {
		t.Fatalf("AllowAll should never error: %v", err)
	}
	if c.ClientID != "anonymous" {
		t.Fatalf("got ClientID %q, want anonymous", c.ClientID)
	}
}

func TestSharedSecret(t *testing.T) {
	auth := NewSharedSecret("s3cr3t")
	tests := []struct {
		name   string
		header string
		ok     bool
	}{
		{"correct", "Bearer s3cr3t", true},
		{"wrong token", "Bearer nope", false},
		{"missing scheme", "s3cr3t", false},
		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.Authenticate(context.Background(), req(tc.header))
			if tc.ok && err != nil {
				t.Fatalf("expected success, got %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("expected failure for %q", tc.header)
			}
		})
	}
}
