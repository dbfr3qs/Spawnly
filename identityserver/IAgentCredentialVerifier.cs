namespace IdentityServer;

/// <summary>
/// The verified caller identity, decoupled from the credential format used to
/// prove it. Mirrors Go's <c>registrant.Identity</c> so both halves of the
/// platform — the registry (self-registration) and this IdentityServer (token
/// minting) — derive the SAME AgentId for the same agent. If they disagree, a
/// token's sub/act won't match the registry record and authorization fails.
/// </summary>
/// <param name="AgentId">
/// The registry-facing identifier (primary key in the agent record).
/// Generalizes today's last-path-segment-of-the-SPIFFE-URI. Non-empty on success.
/// </param>
/// <param name="Subject">
/// The raw verified identity string from the credential (the SPIFFE URI today).
/// This is what lands in the token's <c>act.sub</c> actor chain.
/// </param>
/// <param name="Issuer">
/// Which verifier produced this identity ("spiffe-svid", "aws-sts", ...) — for
/// audit logs and mixed-attestor deployments.
/// </param>
public record AgentIdentity(string AgentId, string Subject, string Issuer);

/// <summary>
/// Verifies an attestation credential (presented as an OAuth
/// <c>client_assertion</c> or <c>actor_token</c>) and derives the agent's
/// identity. The default implementation is SPIFFE/SPIRE
/// (<see cref="SpireCredentialVerifier"/>); other attestors (AWS IRSA, ...)
/// plug in behind the <c>ATTESTOR</c> selector in Program.cs.
/// </summary>
public interface IAgentCredentialVerifier
{
    /// <summary>
    /// Validates the credential's signature and returns the derived identity,
    /// or <c>null</c> if validation fails.
    /// </summary>
    Task<AgentIdentity?> Verify(string credential);
}
