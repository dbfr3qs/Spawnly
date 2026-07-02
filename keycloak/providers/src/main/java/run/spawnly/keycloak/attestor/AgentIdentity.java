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
 */
public record AgentIdentity(String agentId, String subject, String issuer) {
}
