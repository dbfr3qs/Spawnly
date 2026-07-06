// cmd/agent-sidecar/svid_bearer_test.go
package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// mkJWT builds a signature-less JWT with the given exp claim (the sidecar decodes
// exp WITHOUT verifying, so a fake signature segment is fine).
func mkJWT(exp int64) string {
	payload, _ := json.Marshal(map[string]int64{"exp": exp})
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + ".sig"
}

func TestSvidRefreshAt(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	// Refresh is 30s before a comfortably-future exp.
	t.Run("honours exp minus 30s", func(t *testing.T) {
		exp := now.Add(5 * time.Minute)
		got := svidRefreshAt(mkJWT(exp.Unix()), now)
		want := exp.Add(-30 * time.Second)
		if !got.Equal(want) {
			t.Errorf("refresh = %v, want %v", got, want)
		}
	})

	// A token already at/near exp falls back to a soon-but-not-hot-loop window.
	t.Run("near-expiry falls back to 1min", func(t *testing.T) {
		got := svidRefreshAt(mkJWT(now.Add(10*time.Second).Unix()), now)
		if !got.Equal(now.Add(time.Minute)) {
			t.Errorf("near-expiry refresh = %v, want now+1m", got)
		}
	})

	// Unparseable tokens fall back rather than panicking.
	for _, bad := range []string{"", "not-a-jwt", "a.b", "a.!!!.c"} {
		if got := svidRefreshAt(bad, now); !got.Equal(now.Add(time.Minute)) {
			t.Errorf("svidRefreshAt(%q) = %v, want now+1m fallback", bad, got)
		}
	}
}
