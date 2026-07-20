package run.spawnly.keycloak.attestor;

/**
 * The Java mirror of the canonical SPIFFE path contract defined in Go at
 * {@code internal/spiffe/path.go}. Keep the two in LOCKSTEP — the registry
 * (Go), the IdP (this), and the Phase-3 entry creator all derive
 * {trustDomain, orgId, agentId} through the exact same rules, or a token's
 * identity won't line up with the registry record.
 *
 * <h2>The contract</h2>
 * A SPIFFE ID has one of exactly two canonical shapes:
 * <pre>
 *   org-scoped : spiffe://&lt;td&gt;/org/{orgId}/agent/{tenant}/{user}/{type}/{id}
 *   platform   : spiffe://&lt;td&gt;/agent/...                       (org empty)
 * </pre>
 * <ul>
 *   <li>{@code trustDomain} is the URI authority (between {@code spiffe://} and
 *       the first {@code /}).</li>
 *   <li>{@code orgId} is present ONLY for the org-scoped shape: the first path
 *       segment is literally {@code org}, its value is non-empty, and the
 *       following segment is literally {@code agent}. Anything that begins with
 *       {@code /org/} but violates that template — the empty-org
 *       {@code /org//agent/…} spoof, a non-{@code agent} third segment, or a
 *       truncated path — is MALFORMED and rejected (fail closed). A path where
 *       {@code org} merely appears deeper (e.g. {@code /agent/org/…}) carries NO
 *       org claim: {@code orgId} is empty and it is platform-hosted.</li>
 *   <li>{@code agentId} is the LAST path segment in BOTH shapes — Go
 *       {@code path.Base} semantics (trailing slashes stripped), identical to
 *       {@link SpireCredentialVerifier#lastPathSegment(String)}.</li>
 * </ul>
 */
final class SpiffePath {

    private static final String SCHEME = "spiffe://";

    /** Parsed decomposition; {@code orgId} is empty (not null) for platform paths. */
    record Parsed(String trustDomain, String orgId, String agentId) {
    }

    private SpiffePath() {
    }

    /**
     * Decomposes a SPIFFE ID per the canonical contract, or throws
     * {@link IllegalArgumentException} for a non-{@code spiffe://} URI or a path
     * that begins with the org prefix but violates the exact org template. The
     * caller (SpireCredentialVerifier) turns any throw into the oracle-free
     * {@code null} rejection.
     */
    static Parsed parse(String spiffeId) {
        if (spiffeId == null || !spiffeId.startsWith(SCHEME)) {
            throw new IllegalArgumentException("not a spiffe:// URI");
        }
        String rest = spiffeId.substring(SCHEME.length());

        // Trust domain is up to the first '/'; the remainder (with its leading
        // '/') is the path.
        String td;
        String p;
        int slash = rest.indexOf('/');
        if (slash >= 0) {
            td = rest.substring(0, slash);
            p = rest.substring(slash);
        } else {
            td = rest;
            p = "";
        }
        if (td.isEmpty()) {
            throw new IllegalArgumentException("empty trust domain");
        }

        // agentId preserves path.Base semantics EXACTLY (the pre-existing
        // derivation).
        String agentId = lastPathSegment(spiffeId);

        // Org detection. p starts with '/', so split(-1) yields a leading ""
        // element and PRESERVES trailing/empty segments (Java's split drops
        // trailing empties without the negative limit — we need them so the
        // "/org//agent" empty-org spoof is caught, not smoothed over):
        //   "/agent/t/u/ty/id"       -> ["", "agent", "t", ...]
        //   "/org/acme/agent/.../id" -> ["", "org", "acme", "agent", ...]
        String orgId = "";
        String[] segs = p.split("/", -1);
        if (segs.length >= 2 && "org".equals(segs[1])) {
            // The path CLAIMS an org — hold it to the exact template or reject.
            // Minimum legitimate org shape is /org/{org}/agent/{id} — five
            // elements once the leading "" is counted — so a truncated
            // "/org/{org}/agent" with no agent sub-path is malformed.
            if (segs.length < 5 || segs[2].isEmpty() || !"agent".equals(segs[3])) {
                throw new IllegalArgumentException("malformed org-scoped SPIFFE path");
            }
            orgId = segs[2];
        }

        return new Parsed(td, orgId, agentId);
    }

    /**
     * Reproduces Go {@code path.Base} for the last path segment — the canonical
     * agentId derivation shared by the registry ({@code path.Base(spiffeID)}),
     * this parser, and {@link SpireCredentialVerifier}. Trailing slashes are
     * stripped, then everything after the final remaining slash is returned.
     * e.g. {@code spiffe://td/agent/t/u/type/id} -> "id",
     * {@code spiffe://td/agent/id/} -> "id".
     */
    static String lastPathSegment(String s) {
        if (s == null || s.isEmpty()) {
            return "";
        }
        // Strip trailing slashes (path.Base drops trailing separators).
        int end = s.length();
        while (end > 0 && s.charAt(end - 1) == '/') {
            end--;
        }
        if (end == 0) {
            return "/"; // path.Base("///") == "/"; degenerate, treated as empty downstream.
        }
        int slash = s.lastIndexOf('/', end - 1);
        return s.substring(slash + 1, end);
    }
}
