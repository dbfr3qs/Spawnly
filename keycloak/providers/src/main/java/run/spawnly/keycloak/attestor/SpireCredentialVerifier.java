package run.spawnly.keycloak.attestor;

/**
 * SPIFFE/SPIRE attestor. Validates a JWT-SVID against SPIRE's JWKS (via
 * {@link SpireSvidValidator}) and derives {@link AgentIdentity#agentId()} as
 * the last path segment of the SPIFFE URI
 * ({@code spiffe://cluster.local/agent/.../<agentId>}).
 *
 * <p>Ported from identityserver/SpireCredentialVerifier.cs. The agentId
 * derivation MUST match the registry's registrant verifier, which uses Go's
 * {@code path.Base(spiffeID)} (internal/registrant/spiffe.go). This
 * implementation reproduces {@code path.Base} semantics exactly: strip any
 * trailing slashes, then take the substring after the final slash. That is the
 * single source of truth for the derivation on the Keycloak side.
 */
public class SpireCredentialVerifier implements AgentCredentialVerifier {

    private final SpireSvidValidator svid;

    public SpireCredentialVerifier(SpireSvidValidator svid) {
        this.svid = svid;
    }

    @Override
    public AgentIdentity verify(String credential) {
        String spiffeId = svid.validateAndGetSpiffeId(credential);
        if (spiffeId == null || spiffeId.isEmpty()) {
            return null;
        }
        String agentId = lastPathSegment(spiffeId);
        if (agentId == null || agentId.isEmpty()) {
            return null;
        }
        return new AgentIdentity(agentId, spiffeId, "spiffe-svid");
    }

    /**
     * Reproduces Go {@code path.Base} for the last path segment, matching the
     * registry's registrant.SpiffeVerifier ({@code path.Base(spiffeID)}):
     * trailing slashes are stripped, then everything after the final remaining
     * slash is returned. e.g. {@code spiffe://td/agent/t/u/type/id} -> "id",
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
            return "/"; // path.Base("///") == "/"; degenerate, treated as empty by caller downstream anyway.
        }
        int slash = s.lastIndexOf('/', end - 1);
        return s.substring(slash + 1, end);
    }
}
