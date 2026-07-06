package run.spawnly.keycloak.identity;

/**
 * The registry-confirmed agent identity threaded from the client authenticator
 * (which verifies the SVID and runs the liveness gate) to the Phase 2 protocol
 * mappers (which shape the token). This is the single object the mappers read to
 * emit {@code sub=user:<userId>} and {@code act={"sub":"<spiffe>"}}.
 *
 * <p>WHY A SESSION ATTRIBUTE, NOT (only) A CLIENT NOTE (Phase-1 follow-up c):
 * on the {@code client_credentials} grant Keycloak's
 * {@code ClientCredentialsGrantType} creates a FRESH transient authentication
 * session, distinct from the one the client authenticator populated during
 * {@code checkClient()}. So client notes the authenticator wrote do NOT reach
 * the mapper on client_credentials (verified live). What DOES survive is the
 * per-request {@link org.keycloak.models.KeycloakSession} attribute bag: client
 * authentication and token minting run in the SAME KeycloakSession for one token
 * request. The authenticator stashes this object under {@link #SESSION_ATTR};
 * the mapper reads it back. A client-secret client never reaches the
 * authenticator's success path, so it can never populate this attribute — the
 * mapper trust boundary ("only an attestation-authenticated request carries a
 * VerifiedAgent") holds.
 *
 * @param userId  the human the agent acts for (→ {@code sub=user:<userId>})
 * @param subject the verified SPIFFE URI (→ {@code act.sub})
 * @param agentId the registry agent id (the SVID's last path segment)
 * @param orgId   the agent's organization from its registry record (→ the
 *                {@code organization} claim), or {@code null}/empty when the
 *                agent is org-less (global) — the mapper then emits no org claim.
 *                Registry-bound: the authenticator threads {@code agent.orgId()}
 *                here, so an agent can never mint a token claiming another org.
 */
public record VerifiedAgent(String userId, String subject, String agentId, String orgId) {

    /**
     * Key under which the authenticator stashes the {@link VerifiedAgent} in the
     * request-scoped {@code KeycloakSession} attribute bag. Namespaced so it
     * can't collide with Keycloak's own attributes.
     */
    public static final String SESSION_ATTR = "spawnly.verified.agent";
}
