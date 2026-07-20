// internal/spiffe/path.go
//
// The canonical SPIFFE path contract for the platform — ONE shared helper the
// Go registry verifier, the Keycloak Java verifier, and (Phase 3) the entry
// creator MUST all agree on. It is neutral platform code: it depends on nothing
// credential- or transport-specific, and every adapter that speaks SPIFFE
// derives its {trustDomain, orgID, agentID} through this contract so the three
// halves can never drift apart.
//
// # The contract
//
// A SPIFFE ID has one of exactly two canonical shapes:
//
//	org-scoped : spiffe://<td>/org/{orgId}/agent/{tenant}/{user}/{type}/{id}
//	platform   : spiffe://<td>/agent/...                       (org empty)
//
//   - trustDomain is the URI authority (the host between "spiffe://" and the
//     first '/').
//   - orgID is present ONLY for the org-scoped shape: the literal first path
//     segment is "org", its value is non-empty, and the segment that follows is
//     literally "agent". Anything that begins with "/org/" but does not satisfy
//     that exact template (empty org value — the "/org//agent/…" spoof — or a
//     non-"agent" third segment, or a truncated path) is MALFORMED and rejected.
//     A path that is NOT org-scoped (e.g. "/agent/org/…", where "org" merely
//     appears deeper in the tree) carries no org claim: orgID is empty and it is
//     treated as platform-hosted. An attacker therefore cannot smuggle an org
//     claim through a sub-path.
//   - agentID is the LAST path segment in BOTH shapes — Go path.Base(spiffeID)
//     semantics preserved verbatim (trailing slashes stripped), matching the
//     pre-existing internal/registrant/spiffe.go derivation and the Java
//     SpireCredentialVerifier.lastPathSegment. This is load-bearing: the
//     registry record's primary key is this agentID.
//
// The Java mirror lives in
// keycloak/providers/.../attestor/SpiffePath.java — keep the two in lockstep.
package spiffe

import (
	"errors"
	"path"
	"strings"
)

// ParsedSVID is the canonical decomposition of a SPIFFE ID per the path
// contract documented above.
type ParsedSVID struct {
	// TrustDomain is the URI authority, e.g. "cluster.local".
	TrustDomain string
	// OrgID is the owning organization for an org-scoped SVID, or "" for a
	// platform-hosted (org-less) SVID. Never a caller claim — it is derived from
	// the cryptographically-signed SVID path only.
	OrgID string
	// AgentID is the last path segment (path.Base semantics), the registry
	// primary key. Non-empty for a well-formed workload SVID.
	AgentID string
}

// ParsePath decomposes a SPIFFE ID into {TrustDomain, OrgID, AgentID} per the
// canonical contract. It returns an error for anything that is not a
// well-formed spiffe:// URI or that begins with the org-scoped prefix but
// violates the exact org template (fail closed — a malformed org claim is never
// silently downgraded to a platform identity).
func ParsePath(spiffeID string) (ParsedSVID, error) {
	const scheme = "spiffe://"
	if !strings.HasPrefix(spiffeID, scheme) {
		return ParsedSVID{}, errors.New("not a spiffe:// URI")
	}
	rest := spiffeID[len(scheme):]

	// Trust domain is everything up to the first '/'; the remainder (including
	// its leading '/') is the path. A SPIFFE ID with no path is degenerate but
	// still yields a trust domain.
	var td, p string
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		td, p = rest[:i], rest[i:]
	} else {
		td, p = rest, ""
	}
	if td == "" {
		return ParsedSVID{}, errors.New("empty trust domain")
	}

	// agentID preserves path.Base semantics EXACTLY (the pre-existing
	// derivation) so the registry primary key is unchanged for every legacy
	// path shape.
	agentID := path.Base(spiffeID)

	// Org detection. p starts with '/', so Split yields a leading "" element:
	//   "/agent/t/u/ty/id"       -> ["", "agent", "t", "u", "ty", "id"]
	//   "/org/acme/agent/.../id" -> ["", "org", "acme", "agent", ..., "id"]
	// We do NOT collapse empty segments: "/org//agent" -> ["","org","","agent"]
	// so the empty-org spoof is caught, not smoothed over.
	orgID := ""
	if segs := strings.Split(p, "/"); len(segs) >= 2 && segs[1] == "org" {
		// The path CLAIMS an org — hold it to the exact template or reject.
		// Minimum legitimate org shape is /org/{org}/agent/{id} — five elements
		// once the leading "" is counted — so a truncated "/org/{org}/agent"
		// with no agent sub-path is malformed, not a valid identity.
		if len(segs) < 5 || segs[2] == "" || segs[3] != "agent" {
			return ParsedSVID{}, errors.New("malformed org-scoped SPIFFE path")
		}
		orgID = segs[2]
	}

	return ParsedSVID{TrustDomain: td, OrgID: orgID, AgentID: agentID}, nil
}
