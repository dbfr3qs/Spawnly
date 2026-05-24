using Duende.IdentityServer.Models;

namespace IdentityServer;

public static class Config
{
    public static IEnumerable<ApiScope> ApiScopes =>
        new List<ApiScope> { new ApiScope("sample-api", "Sample API") };

    public static IEnumerable<Client> Clients() =>
        new List<Client>
        {
            new Client
            {
                ClientId = "worker",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api" },
            }
        };
}
