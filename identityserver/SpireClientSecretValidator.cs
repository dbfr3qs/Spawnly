using Duende.IdentityServer.Validation;
using Duende.IdentityServer.Stores;

namespace IdentityServer;

/// <summary>
/// Top-level client authentication at the token endpoint. Machine clients
/// (agents) authenticate with a SPIFFE JWT-SVID presented as a
/// <c>client_assertion</c>; this validator accepts those by verifying the SVID
/// against SPIRE's JWKS.
///
/// Any request that is NOT a SPIFFE client_assertion (e.g. the interactive
/// <c>dashboard</c> client doing authorization_code with a real client_secret)
/// is delegated to Duende's built-in <see cref="ClientSecretValidator"/>, which
/// performs normal secret validation. This lets human login coexist with the
/// machine-identity flows on a single token endpoint.
/// </summary>
public class SpireClientSecretValidator : IClientSecretValidator
{
    private readonly IClientStore _clients;
    private readonly SpireSvidValidator _svid;
    private readonly ClientSecretValidator _inner;

    public SpireClientSecretValidator(
        IClientStore clients,
        SpireSvidValidator svid,
        ClientSecretValidator inner)
    {
        _clients = clients;
        _svid = svid;
        _inner = inner;
    }

    public async Task<ClientSecretValidationResult> ValidateAsync(HttpContext context)
    {
        var form = await context.Request.ReadFormAsync();
        var clientId = form["client_id"].FirstOrDefault();
        var assertion = form["client_assertion"].FirstOrDefault();
        var assertionType = form["client_assertion_type"].FirstOrDefault();

        // Not a SPIFFE assertion — hand off to Duende's default secret validation
        // (client_secret_post / basic). This is the human-login (dashboard) path.
        if (assertion is null ||
            assertionType != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
            return await _inner.ValidateAsync(context);

        if (clientId is null) return Fail();

        var client = await _clients.FindClientByIdAsync(clientId);
        if (client is null) return Fail();

        var spiffeId = await _svid.ValidateAndGetSpiffeId(assertion);
        if (spiffeId is null) return Fail();

        return new ClientSecretValidationResult { IsError = false, Client = client };
    }

    private static ClientSecretValidationResult Fail() =>
        new ClientSecretValidationResult { IsError = true };
}
