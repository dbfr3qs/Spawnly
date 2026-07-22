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

    private static final org.jboss.logging.Logger log =
            org.jboss.logging.Logger.getLogger(SpireCredentialVerifier.class);

    private final SpireSvidValidator svid;

    /**
     * The expected SPIFFE trust domain (remote-substrate Phase 1). When set (the
     * production default is "cluster.local", wired from {@code SPIRE_TRUST_DOMAIN}
     * in {@link AttestorFactory}), {@link #verify} parses the validated SVID
     * through the canonical {@link SpiffePath} contract, REJECTS any SVID whose
     * trust domain differs, and surfaces the org segment on
     * {@link AgentIdentity#orgId()} so {@link OrgAwareVerifier} can bind it to the
     * record's org.
     *
     * <p>When EMPTY/null (used by unit tests that mint SVIDs in arbitrary trust
     * domains via the bare constructor), the verifier stays in LEGACY passthrough
     * mode: agentId = last path segment, orgId = null — bit-identical to the
     * pre-substrate behaviour. Mirrors the Go
     * {@code registrant.SpiffeVerifier.TrustDomain} empty-means-legacy switch.
     * Note: this passthrough is NOT env-selectable — {@link AttestorFactory}
     * resolves an empty {@code SPIRE_TRUST_DOMAIN} to the "cluster.local" default,
     * so production always constructs the enforcing form.
     */
    private final String trustDomain;

    /** Legacy ctor: trust-domain enforcement OFF (org-less passthrough). */
    public SpireCredentialVerifier(SpireSvidValidator svid) {
        this(svid, null);
    }

    /** Enforcing ctor: pin {@code trustDomain} and surface the org segment. */
    public SpireCredentialVerifier(SpireSvidValidator svid, String trustDomain) {
        this.svid = svid;
        this.trustDomain = (trustDomain == null || trustDomain.isEmpty()) ? null : trustDomain;
    }

    @Override
    public AgentIdentity verify(String credential) {
        String spiffeId = svid.validateAndGetSpiffeId(credential);
        if (spiffeId == null || spiffeId.isEmpty()) {
            return null;
        }

        // Legacy passthrough: no expected trust domain ⇒ exactly today's
        // behaviour (last path segment, no org).
        if (trustDomain == null) {
            String legacyId = lastPathSegment(spiffeId);
            if (legacyId == null || legacyId.isEmpty()) {
                return null;
            }
            return new AgentIdentity(legacyId, spiffeId, "spiffe-svid");
        }

        // Enforcing mode: parse through the canonical contract, pin the trust
        // domain, surface the org. Any parse failure (incl. the "/org//agent/…"
        // empty-org spoof) or a trust-domain mismatch ⇒ oracle-free null.
        SpiffePath.Parsed parsed;
        try {
            parsed = SpiffePath.parse(spiffeId);
        } catch (RuntimeException e) {
            log.debugf(e, "SPIRE SVID path rejected: %s", spiffeId);
            return null;
        }
        if (!trustDomain.equals(parsed.trustDomain())) {
            log.debugf("SPIRE SVID trust domain %s not permitted (expected %s)",
                    parsed.trustDomain(), trustDomain);
            return null;
        }
        String agentId = parsed.agentId(); // == lastPathSegment, contract-preserved
        if (agentId == null || agentId.isEmpty()) {
            return null;
        }
        if (parsed.orgId().isEmpty()) {
            // Genuinely platform (org-less) path in ENFORCING mode: flag it so the
            // OrgAwareVerifier applies the reverse org-binding. Never flagged in
            // legacy passthrough above.
            return new AgentIdentity(agentId, spiffeId, "spiffe-svid", null, true);
        }
        return new AgentIdentity(agentId, spiffeId, "spiffe-svid", parsed.orgId());
    }

    /**
     * The canonical last-path-segment (Go {@code path.Base}) derivation, matching
     * the registry's registrant.SpiffeVerifier. Delegates to the neutral
     * {@link SpiffePath#lastPathSegment(String)} so the contract lives in one
     * place (dependency direction: the verifier depends on the parser, not the
     * reverse). Retained here as the token-exchange provider's parity reference
     * and this class's own test vector entry point.
     */
    static String lastPathSegment(String s) {
        return SpiffePath.lastPathSegment(s);
    }
}
