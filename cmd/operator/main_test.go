// cmd/operator/main_test.go
package main

import "testing"

// TestControlPlaneToken verifies the operator only presents a bearer to the
// registry's control-plane endpoints under the shared-secret tier; none/unset
// yields "" (open demo tier).
func TestControlPlaneToken(t *testing.T) {
	tests := []struct {
		name  string
		auth  string
		token string
		want  string
	}{
		{"shared-secret returns token", "shared-secret", "tok", "tok"},
		{"none returns empty", "none", "tok", ""},
		{"unset returns empty", "", "tok", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONTROL_PLANE_AUTH", tt.auth)
			t.Setenv("CONTROL_PLANE_TOKEN", tt.token)
			if got := controlPlaneToken(); got != tt.want {
				t.Fatalf("controlPlaneToken() = %q, want %q", got, tt.want)
			}
		})
	}
}
