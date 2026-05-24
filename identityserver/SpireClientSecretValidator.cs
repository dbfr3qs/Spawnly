using Duende.IdentityServer.Models;
using Duende.IdentityServer.Validation;
using Microsoft.IdentityModel.JsonWebTokens;
using Microsoft.IdentityModel.Tokens;

namespace IdentityServer;

public class SpireClientSecretValidator : IClientSecretValidator
{
    private readonly IClientStore _clients;
    private readonly IHttpClientFactory _httpFactory;
    private readonly string _spireJwksUrl;

    public SpireClientSecretValidator(
        IClientStore clients, IHttpClientFactory httpFactory, string spireJwksUrl)
    {
        _clients = clients;
        _httpFactory = httpFactory;
        _spireJwksUrl = spireJwksUrl;
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

        var http = _httpFactory.CreateClient();
        var jwksJson = await http.GetStringAsync(_spireJwksUrl);
        var jwks = new JsonWebKeySet(jwksJson);

        var handler = new JsonWebTokenHandler();
        var result = await handler.ValidateTokenAsync(assertion, new TokenValidationParameters
        {
            ValidateAudience = false,
            ValidateIssuer = false,
            IssuerSigningKeys = jwks.GetSigningKeys(),
        });

        if (!result.IsValid) return Fail();

        return new ClientSecretValidationResult { IsError = false, Client = client };
    }

    private static ClientSecretValidationResult Fail() =>
        new ClientSecretValidationResult { IsError = true };
}
