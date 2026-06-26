package registry

import (
	"testing"
	"time"
)

// TestNewClientTimeout pins the registry HTTP client's request timeout so a hung
// registry can't block a caller goroutine indefinitely. White-box (internal
// package) so it can read the unexported http client — the behavioral hang test
// in client_test.go proves give-up, this guards the exact value from a revert.
func TestNewClientTimeout(t *testing.T) {
	if got := New("http://example").http.Timeout; got != 30*time.Second {
		t.Fatalf("registry client Timeout = %v, want 30s", got)
	}
}
