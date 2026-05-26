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
                // Placeholder so Duende's config validator is satisfied;
                // actual auth is via SpireClientSecretValidator (client_assertion JWT-SVID).
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api" },
            },
            new Client
            {
                ClientId = "weather-monitor",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api" },
            }
        };
}
