package run.spawnly.keycloak.attestor;

/**
 * Verifies an attestation credential (presented as an OAuth
 * {@code client_assertion} or, in Phase 3, an {@code actor_token}) and derives
 * the agent's identity. Ported from
 * identityserver/IAgentCredentialVerifier.cs.
 *
 * <p>The default implementation is SPIFFE/SPIRE
 * ({@link SpireCredentialVerifier}); other attestors (AWS STS, AWS STS web
 * identity) plug in behind the {@code ATTESTOR} selector in
 * {@link AttestorFactory}, arriving in Phase 5.
 *
 * <p>DEPENDENCY DIRECTION: implementations depend only on the neutral
 * credential formats + the SPIRE/STS HTTP surface, never on Keycloak internals.
 */
public interface AgentCredentialVerifier {

    /**
     * Validates the credential's signature and returns the derived identity,
     * or {@code null} if validation fails. Never throws for a bad credential —
     * a null return is the single "not authenticated" signal (matching the C#
     * {@code Verify} returning a nullable {@code AgentIdentity}).
     *
     * @param credential the raw client_assertion value (a JWT-SVID for SPIFFE)
     */
    AgentIdentity verify(String credential);
}
