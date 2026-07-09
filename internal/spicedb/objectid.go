package spicedb

import "strings"

// EncodeObjectID maps an arbitrary identifier (a userId/orgId that flows into a
// SpiceDB tuple object id) to a string that satisfies SpiceDB's object-id
// charset. SpiceDB's default object id grammar is roughly
// `[a-zA-Z0-9/_|=+-]+`; the platform's identity subjects are broader —
// registry.ValidUserID admits `. : @` (so "user:alice", UUIDs, and email-style
// usernames all pass) and validAgentID admits `.`. Writing such a raw value as
// an object id ("user:alice@x.com") would be REJECTED by SpiceDB, so a tuple
// write silently fails and a later CheckPermission never matches.
//
// The encoding is:
//   - a deterministic, pure function (same input → same output);
//   - injective (distinct inputs never collide — a collision would let one
//     user's grant satisfy another user's permission check), because the only
//     way to emit '=' is the escape sequence and passthrough bytes never
//     include '=';
//   - the IDENTITY on the already-safe subset [A-Za-z0-9_-], so Keycloak UUID
//     subs, DNS-label org ids, and SPIFFE-derived agent ids are unchanged (the
//     live cluster's seeded tuples are byte-for-byte identical with or without
//     encoding — encoding only ever transforms the exotic chars).
//
// It MUST be applied identically on the WRITE side (owner/org agent tuples and
// org membership tuples) and the READ side (the Phase-4 CheckPermission
// subject/resource) so the two agree. Everything outside the passthrough set is
// escaped as "=" followed by two uppercase hex digits of the byte.
func EncodeObjectID(raw string) string {
	// Fast path: return raw unchanged when every byte is already safe (the
	// common case for UUID subs / DNS-label org ids), avoiding an allocation.
	if isAllSafe(raw) {
		return raw
	}
	var b strings.Builder
	b.Grow(len(raw) + 8)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if safeObjectIDByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('=')
		b.WriteByte(hexDigit(c >> 4))
		b.WriteByte(hexDigit(c & 0x0f))
	}
	return b.String()
}

func isAllSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		if !safeObjectIDByte(s[i]) {
			return false
		}
	}
	return true
}

// safeObjectIDByte reports whether c may pass through unescaped. It is the
// conservative always-safe subset of SpiceDB's object-id charset and, crucially,
// EXCLUDES '=' so the escape introducer can never appear in passthrough output
// (keeping the encoding unambiguous / injective).
func safeObjectIDByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-':
		return true
	default:
		return false
	}
}

func hexDigit(n byte) byte {
	if n < 10 {
		return '0' + n
	}
	return 'A' + (n - 10)
}
