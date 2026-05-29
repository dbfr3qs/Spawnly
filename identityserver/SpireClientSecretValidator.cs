using Duende.IdentityServer.Validation;
using Duende.IdentityServer.Stores;

namespace IdentityServer;

public class SpireClientSecretValidator : IClientSecretValidator
{
    private readonly IClientStore _clients;
    private readonly SpireSvidValidator _svid;

    public SpireClientSecretValidator(IClientStore clients, SpireSvidValidator svid)
    {
        _clients = clients;
        _svid = svid;
    }

    public async Task<ClientSecretValidationResult> ValidateAsync(HttpContext context)
    {
        var form = await context.Request.ReadFormAsync();
        var clientId = form["client_id"].FirstOrDefault();
        var assertion = form["client_assertion"].FirstOrDefault();
        var assertionType = form["client_assertion_type"].FirstOrDefault();

        if (clientId is null || assertion is null ||
            assertionType != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
            return Fail();

        var client = await _clients.FindClientByIdAsync(clientId);
        if (client is null) return Fail();

        var spiffeId = await _svid.ValidateAndGetSpiffeId(assertion);
        if (spiffeId is null) return Fail();

        return new ClientSecretValidationResult { IsError = false, Client = client };
    }

    private static ClientSecretValidationResult Fail() =>
        new ClientSecretValidationResult { IsError = true };
}
