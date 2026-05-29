using Microsoft.IdentityModel.JsonWebTokens;
using Microsoft.IdentityModel.Tokens;

namespace IdentityServer;

/// <summary>
/// Validates SPIRE JWT-SVIDs against the SPIRE OIDC discovery provider JWKS.
/// Shared by the client-secret validator (client_assertion) and the
/// token-exchange extension grant (actor_token).
/// </summary>
public class SpireSvidValidator
{
    private readonly IHttpClientFactory _httpFactory;
    private readonly string _spireJwksUrl;

    public SpireSvidValidator(IHttpClientFactory httpFactory, string spireJwksUrl)
    {
        _httpFactory = httpFactory;
        _spireJwksUrl = spireJwksUrl;
    }

    /// <summary>
    /// Validates the signature of a SPIRE JWT-SVID against the SPIRE JWKS.
    /// Returns the SPIFFE URI (the token's subject) on success, or null on failure.
    /// </summary>
    public async Task<string?> ValidateAndGetSpiffeId(string svid)
    {
        if (string.IsNullOrEmpty(svid)) return null;

        try
        {
            // SPIRE OIDC provider uses a self-signed cert — use named client with TLS bypass.
            var http = _httpFactory.CreateClient("spire");
            var jwksJson = await http.GetStringAsync(_spireJwksUrl);
            var jwks = new JsonWebKeySet(jwksJson);

            var handler = new JsonWebTokenHandler();
            var result = await handler.ValidateTokenAsync(svid, new TokenValidationParameters
            {
                ValidateAudience = false,
                ValidateIssuer = false,
                IssuerSigningKeys = jwks.GetSigningKeys(),
            });

            if (!result.IsValid) return null;

            var jwt = handler.ReadJsonWebToken(svid);
            return jwt.Subject;
        }
        catch
        {
            return null;
        }
    }
}
