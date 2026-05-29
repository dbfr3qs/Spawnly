using Duende.IdentityServer.Models;

namespace IdentityServer;

public static class Config
{
    public const string TokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange";

    public static IEnumerable<ApiScope> ApiScopes =>
        new List<ApiScope>
        {
            new ApiScope("sample-api", "Sample API"), // backward compat
            new ApiScope("sample-api-a:read", "Read sample-api-a"),
            new ApiScope("sample-api-a:write", "Write sample-api-a"),
            new ApiScope("sample-api-b:read", "Read sample-api-b"),
            new ApiScope("sample-api-b:write", "Write sample-api-b"),
        };

    // ApiResources give the access token its audience (`aud`): the resource name is
    // emitted as an audience when one of its scopes is granted.
    public static IEnumerable<ApiResource> ApiResources =>
        new List<ApiResource>
        {
            new ApiResource("sample-api-a", "Sample API A")
            {
                Scopes = { "sample-api-a:read", "sample-api-a:write" },
            },
            new ApiResource("sample-api-b", "Sample API B")
            {
                Scopes = { "sample-api-b:read", "sample-api-b:write" },
            },
        };

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
            },
            new Client
            {
                ClientId = "parent-agent",
                // Both client_credentials (to mint a root token) and token-exchange
                // (to delegate to a child / re-exchange).
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    TokenExchangeGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes =
                {
                    "sample-api-a:read",
                    "sample-api-a:write",
                    "sample-api-b:read",
                },
            },
            new Client
            {
                ClientId = "child-agent",
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    TokenExchangeGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes =
                {
                    "sample-api-b:read",
                },
            },
        };
}
