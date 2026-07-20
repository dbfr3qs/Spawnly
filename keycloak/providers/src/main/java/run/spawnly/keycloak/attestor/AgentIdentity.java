package run.spawnly.keycloak.attestor;

/**
 * The verified caller identity, decoupled from the credential format used to
 * prove it. Ported from identityserver/IAgentCredentialVerifier.cs
 * ({@code AgentIdentity}); mirrors Go's {@code registrant.Identity} so both
 * halves of the platform — the registry (self-registration) and the IdP
 * (token minting) — derive the SAME agentId for the same agent. If they
 * disagree, a token's sub/act won't match the registry record and
 * authorization fails.
 *
 * @param agentId the registry-facing identifier (primary key in the agent
 *                record) — today the last path segment of the SPIFFE URI.
 *                Non-empty on success.
 * @param subject the raw verified identity string from the credential (the
 *                SPIFFE URI today). This is what lands in the token's
 *                {@code act.sub} actor chain (Phase 2).
 * @param issuer  which verifier produced this identity ("spiffe-svid",
 *                "aws-sts", ...) — for audit logs and mixed-attestor deploys.
 * @param orgId   the owning organization derived from an org-scoped SPIFFE path
 *                (spiffe://&lt;td&gt;/org/{orgId}/agent/…), or {@code null} for a
 *                platform-hosted (org-less) SVID and for non-SPIFFE credentials.
 *                NEVER a caller claim — it comes only from the
 *                cryptographically-signed SVID path. {@link OrgAwareVerifier}
 *                binds a non-null orgId to the agent record's org, so an org-A
 *                SVID can never authenticate an agent recorded under org B.
 *                Mirrors Go {@code registrant.Identity.OrgID}.
 * @param platformSvid set true ONLY by the trust-domain-ENFORCING
 *                {@link SpireCredentialVerifier} for a genuinely platform
 *                (org-less) SPIFFE path — i.e. orgId is null because the path is
 *                {@code /agent/…}, not because org parsing was off. It gates the
 *                reverse org-binding in {@link OrgAwareVerifier} (a platform SVID
 *                must not authenticate an org-scoped record). NEVER set in legacy
 *                passthrough or by non-SPIFFE verifiers, so their behaviour is
 *                bit-identical. Mirrors Go {@code registrant.Identity.platformSVID}.
 */
public record AgentIdentity(String agentId, String subject, String issuer, String orgId, boolean platformSvid) {

    /**
     * Backward-compatible constructor for the org-less case and the many call
     * sites (STS/stsweb verifiers, tests) that predate remote-substrate:
     * delegates to the canonical constructor with {@code orgId=null} and
     * {@code platformSvid=false}.
     */
    public AgentIdentity(String agentId, String subject, String issuer) {
        this(agentId, subject, issuer, null, false);
    }

    /**
     * Backward-compatible constructor carrying an org but not the platform flag
     * (org-scoped identities never need it — the reverse binding is org-less only).
     */
    public AgentIdentity(String agentId, String subject, String issuer, String orgId) {
        this(agentId, subject, issuer, orgId, false);
    }
}
