package spicedb

import "testing"

func TestEncodeObjectID_IdentityOnSafeInput(t *testing.T) {
	// Keycloak UUID subs, DNS-label org ids, and SPIFFE-derived agent ids are
	// already SpiceDB-safe and MUST pass through byte-for-byte unchanged, so the
	// live cluster's seeded tuples are identical with or without encoding.
	for _, in := range []string{
		"a1b2c3d4-1234-5678-9abc-def012345678", // Keycloak UUID sub
		"acme", "globex-2", "my-org",           // DNS-label org ids
		"chain-worker-ab12cd", "agent-9f8e7d", // SPIFFE-derived agent ids
		"alice", "viewer", "bob",
		"", // empty stays empty
	} {
		if got := EncodeObjectID(in); got != in {
			t.Errorf("EncodeObjectID(%q) = %q, want identity", in, got)
		}
	}
}

func TestEncodeObjectID_EscapesExoticChars(t *testing.T) {
	cases := map[string]string{
		"user:alice":    "user=3Aalice",
		"alice@x.com":   "alice=40x=2Ecom",
		"a.b":           "a=2Eb",
		"has space":     "has=20space",
		"colon:at@dot.": "colon=3Aat=40dot=2E",
	}
	for in, want := range cases {
		if got := EncodeObjectID(in); got != want {
			t.Errorf("EncodeObjectID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeObjectID_Injective(t *testing.T) {
	// Distinct inputs must never collide — a collision would let one subject's
	// grant satisfy another's permission check. Spot-check pairs that look close.
	pairs := [][2]string{
		{"a.b", "a=2Eb"}, // "a.b" encodes to "a=2Eb"; the literal "a=2Eb" must differ
		{"user:a", "user:b"},
		{"a@b", "a:b"},
		{"", "a"},
	}
	for _, p := range pairs {
		if EncodeObjectID(p[0]) == EncodeObjectID(p[1]) {
			t.Errorf("collision: EncodeObjectID(%q) == EncodeObjectID(%q)", p[0], p[1])
		}
	}
}

func TestEncodeObjectID_OutputIsSpiceDBSafe(t *testing.T) {
	for _, in := range []string{"user:alice@example.com", "weird/id|x", "a.b.c", "空"} {
		out := EncodeObjectID(in)
		for i := 0; i < len(out); i++ {
			c := out[i]
			ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
				(c >= '0' && c <= '9') || c == '_' || c == '-' || c == '='
			if !ok {
				t.Errorf("EncodeObjectID(%q) = %q contains unsafe byte %q", in, out, string(c))
			}
		}
	}
}
