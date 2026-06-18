namespace IdentityServer;

/// <summary>
/// SPIFFE/SPIRE attestor. Validates a JWT-SVID against SPIRE's JWKS (via
/// <see cref="SpireSvidValidator"/>) and derives <see cref="AgentIdentity.AgentId"/>
/// as the last path segment of the SPIFFE URI
/// (spiffe://cluster.local/agent/.../&lt;agentId&gt;) — the single place that
/// derivation now lives, previously duplicated across the token validators.
/// </summary>
public class SpireCredentialVerifier : IAgentCredentialVerifier
{
    private readonly SpireSvidValidator _svid;

    public SpireCredentialVerifier(SpireSvidValidator svid) => _svid = svid;

    public async Task<AgentIdentity?> Verify(string credential)
    {
        var spiffeId = await _svid.ValidateAndGetSpiffeId(credential);
        if (string.IsNullOrEmpty(spiffeId)) return null;

        var agentId = spiffeId.Split('/', StringSplitOptions.RemoveEmptyEntries).LastOrDefault();
        if (string.IsNullOrEmpty(agentId)) return null;

        return new AgentIdentity(agentId, spiffeId, "spiffe-svid");
    }
}
