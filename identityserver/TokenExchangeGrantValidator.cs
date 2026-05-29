using Duende.IdentityServer.Models;
using Duende.IdentityServer.Services;
using Duende.IdentityServer.Validation;
using Microsoft.IdentityModel.JsonWebTokens;
using Microsoft.IdentityModel.Tokens;
using System.Security.Claims;
using System.Text.Json;

namespace IdentityServer;

/// <summary>
/// RFC 8693 OAuth2 Token Exchange.
///
/// Flow: an agent (the actor, authenticated by its SPIRE JWT-SVID in
/// <c>actor_token</c>) exchanges a token THIS IdentityServer previously issued
/// (<c>subject_token</c>) for a new token, optionally narrowing scope and
/// retargeting the audience.
///
/// Identity carried in the new token:
///   sub = subject_token.sub (the user, carried unchanged)
///   act = { "sub": "&lt;actor spiffe&gt;", "act": &lt;subject_token.act&gt; }
///         (the new actor is wrapped around the previous delegation chain)
///   scope = requested ∩ subject_token.scope
///   aud   = the requested audience (see <see cref="DelegationAudience"/>)
/// </summary>
public class TokenExchangeGrantValidator : IExtensionGrantValidator
{
    /// <summary>
    /// Sentinel audience meaning "not for any resource server — only re-exchangeable at IS".
    /// Because Duende derives the JWT `aud` from matched API resources and filters any custom
    /// `aud` claim, a delegation token simply carries NO resource audience. We additionally
    /// emit `token_use: delegation` as the authoritative signal for resource servers to reject.
    /// </summary>
    public const string DelegationAudience = "delegation";

    public string GrantType => Config.TokenExchangeGrantType;

    private const string AccessTokenType = "urn:ietf:params:oauth:token-type:access_token";
    private const string JwtTokenType = "urn:ietf:params:oauth:token-type:jwt";

    private readonly SpireSvidValidator _svid;
    private readonly IKeyMaterialService _keys;
    private readonly IIssuerNameService _issuer;

    public TokenExchangeGrantValidator(
        SpireSvidValidator svid, IKeyMaterialService keys, IIssuerNameService issuer)
    {
        _svid = svid;
        _keys = keys;
        _issuer = issuer;
    }

    public async Task ValidateAsync(ExtensionGrantValidationContext context)
    {
        var raw = context.Request.Raw;

        var subjectToken = raw.Get("subject_token");
        var subjectTokenType = raw.Get("subject_token_type");
        var actorToken = raw.Get("actor_token");
        var actorTokenType = raw.Get("actor_token_type");
        var requestedAudience = raw.Get("audience");
        var requestedScope = raw.Get("scope");

        if (string.IsNullOrEmpty(subjectToken))
        {
            context.Result = Error("subject_token is required");
            return;
        }

        if (!IsAccessTokenType(subjectTokenType))
        {
            context.Result = Error("unsupported subject_token_type");
            return;
        }

        if (string.IsNullOrEmpty(actorToken))
        {
            context.Result = Error("actor_token is required");
            return;
        }

        if (!IsAccessTokenType(actorTokenType))
        {
            context.Result = Error("unsupported actor_token_type");
            return;
        }

        // 1. actor_token must be a valid SPIRE SVID. Its subject is the new actor.
        var actorSpiffe = await _svid.ValidateAndGetSpiffeId(actorToken);
        if (actorSpiffe is null)
        {
            context.Result = Error("invalid actor_token (SVID validation failed)");
            return;
        }

        // 2. subject_token must be a token THIS IdentityServer issued.
        var subject = await ValidateSubjectToken(subjectToken);
        if (subject is null)
        {
            context.Result = Error("invalid subject_token");
            return;
        }

        if (!subject.TryGetClaim("sub", out var subClaim) || string.IsNullOrEmpty(subClaim.Value))
        {
            context.Result = Error("subject_token has no sub");
            return;
        }
        var sub = subClaim.Value;

        // 3. scope = requested ∩ subject_token.scope (registry policy ceiling: later milestone).
        //    Duende issues exactly the scopes from the request's `scope` param (validated
        //    against the client's AllowedScopes) — it ignores anything we compute here. So to
        //    guarantee issued scope ⊆ subject scope we REJECT if any requested scope is absent
        //    from the subject token. `scope` is therefore required for an exchange (omitting it
        //    would otherwise let Duende issue the client's full allowed set, exceeding the
        //    subject token's grant).
        var subjectScopes = GetScopes(subject).ToHashSet();
        var requestedScopes = (requestedScope ?? string.Empty)
            .Split(' ', StringSplitOptions.RemoveEmptyEntries)
            .ToList();

        if (requestedScopes.Count == 0)
        {
            context.Result = Error("scope is required for token exchange");
            return;
        }

        var disallowed = requestedScopes.Where(s => !subjectScopes.Contains(s)).ToList();
        if (disallowed.Count > 0)
        {
            context.Result = Error(
                $"invalid_scope: requested scope(s) not present in subject_token: {string.Join(' ', disallowed)}");
            return;
        }

        // 4. act = { "sub": "<actor spiffe>", "act": <subject_token's act> }.
        //    Wrap the new actor around the previous delegation chain.
        //
        //    IMPORTANT: emit act (and token_use) via the request's ClientClaims, NOT via the
        //    GrantValidationResult claims. Duende's DefaultClaimsService filters arbitrary
        //    result claims out of the access token, but ClientClaims are emitted verbatim
        //    (the clients use ClientClaimsPrefix=""). This is the same channel the root
        //    client_credentials token uses for its act claim.
        var act = BuildActClaim(actorSpiffe, subject);
        context.Request.ClientClaims.Add(new Claim("act", act, JsonClaimValueTypes.Json));

        // 5. Audience.
        //    Real resources (e.g. sample-api-b): the granted scopes map to that ApiResource,
        //    so Duende emits the correct `aud` automatically — nothing to do here.
        //    Delegation: emit token_use=delegation; resource servers reject it, so it is only
        //    re-exchangeable at IS (used when delegating further down a chain).
        if (string.Equals(requestedAudience, DelegationAudience, StringComparison.Ordinal))
        {
            context.Request.ClientClaims.Add(new Claim("token_use", DelegationAudience));
        }

        // Duende issues the request's validated scopes; we have already rejected any requested
        // scope absent from the subject token, so issued scope ⊆ subject scope holds.
        context.Result = new GrantValidationResult(
            subject: sub,
            authenticationMethod: GrantType);
    }

    private static bool IsAccessTokenType(string? type) =>
        type == AccessTokenType || type == JwtTokenType;

    /// <summary>
    /// Validates the subject_token's signature against this IdentityServer's signing keys
    /// and confirms the issuer matches. Returns the parsed token on success, else null.
    /// </summary>
    private async Task<JsonWebToken?> ValidateSubjectToken(string token)
    {
        try
        {
            var issuer = await _issuer.GetCurrentAsync();
            var keys = await _keys.GetValidationKeysAsync();
            var signingKeys = keys.Select(k => k.Key).ToList();

            var handler = new JsonWebTokenHandler();
            var result = await handler.ValidateTokenAsync(token, new TokenValidationParameters
            {
                ValidIssuer = issuer,
                ValidateIssuer = true,
                ValidateAudience = false, // a delegation token has no resource audience
                ValidateLifetime = true,
                IssuerSigningKeys = signingKeys,
            });

            if (!result.IsValid) return null;
            return handler.ReadJsonWebToken(token);
        }
        catch
        {
            return null;
        }
    }

    private static List<string> GetScopes(JsonWebToken token)
    {
        // Duende emits `scope` as repeated claims (one per scope) in the JWT payload array.
        var scopes = token.Claims
            .Where(c => c.Type == "scope")
            .SelectMany(c => c.Value.Split(' ', StringSplitOptions.RemoveEmptyEntries))
            .Distinct()
            .ToList();
        return scopes;
    }

    /// <summary>
    /// Builds the new act claim as a JSON object string:
    /// { "sub": "&lt;actor&gt;", "act": &lt;previous act&gt; }.
    /// The previous act (if any) is embedded verbatim as a nested object so the
    /// full delegation chain is preserved.
    /// </summary>
    private static string BuildActClaim(string actorSpiffe, JsonWebToken subject)
    {
        using var stream = new MemoryStream();
        using (var writer = new Utf8JsonWriter(stream))
        {
            writer.WriteStartObject();
            writer.WriteString("sub", actorSpiffe);

            // Carry the previous act (the subject_token's actor chain), if present, as nested JSON.
            if (subject.TryGetClaim("act", out var actClaim) && !string.IsNullOrEmpty(actClaim.Value))
            {
                writer.WritePropertyName("act");
                using var prev = JsonDocument.Parse(actClaim.Value);
                prev.RootElement.WriteTo(writer);
            }

            writer.WriteEndObject();
        }
        return System.Text.Encoding.UTF8.GetString(stream.ToArray());
    }

    private static GrantValidationResult Error(string description) =>
        new GrantValidationResult(TokenRequestErrors.InvalidRequest, description);
}
