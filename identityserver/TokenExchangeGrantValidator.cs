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
    private readonly AgentRegistryClient _registry;

    public TokenExchangeGrantValidator(
        SpireSvidValidator svid, IKeyMaterialService keys, IIssuerNameService issuer,
        AgentRegistryClient registry)
    {
        _svid = svid;
        _keys = keys;
        _issuer = issuer;
        _registry = registry;
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

        // 3b. Delegation-policy enforcement (Milestone 2).
        //
        //     The exchange creates a NEW delegation edge: the immediate delegator (parent) is
        //     the OUTERMOST actor already named in the subject_token's act chain; the delegate
        //     (child) is the actor performing this exchange (actor_token's SPIFFE id). The
        //     registry policy for that (parentType -> childType) edge gates whether the edge is
        //     allowed, caps the scopes that may be carried across it, and bounds chain depth.

        // Resolve the child (the actor doing the exchange): last path segment of its SPIFFE URI.
        var childAgentId = LastPathSegment(actorSpiffe);
        // Resolve the parent (the immediate delegator): outermost act.sub of the subject_token.
        var parentSpiffe = OutermostActSub(subject);
        if (string.IsNullOrEmpty(parentSpiffe))
        {
            context.Result = PolicyError("cannot resolve delegation parties");
            return;
        }
        var parentAgentId = LastPathSegment(parentSpiffe);

        if (string.IsNullOrEmpty(childAgentId) || string.IsNullOrEmpty(parentAgentId))
        {
            context.Result = PolicyError("cannot resolve delegation parties");
            return;
        }

        var childAgent = await _registry.GetAgent(childAgentId);
        var parentAgent = await _registry.GetAgent(parentAgentId);
        var childType = childAgent?.AgentType;
        var parentType = parentAgent?.AgentType;
        if (string.IsNullOrEmpty(childType) || string.IsNullOrEmpty(parentType))
        {
            context.Result = PolicyError("cannot resolve delegation parties");
            return;
        }

        var policy = await _registry.GetDelegationPolicy(parentType, childType);
        if (policy is null || !policy.Allowed)
        {
            context.Result = PolicyError($"delegation not permitted: {parentType} -> {childType}");
            return;
        }

        // Scope ceiling: every requested scope must be grantable across this edge. This is in
        // addition to the requested ⊆ subject_token.scope check above.
        var grantableScopes = (policy.GrantableScopes ?? new List<string>()).ToHashSet();
        foreach (var s in requestedScopes)
        {
            if (!grantableScopes.Contains(s))
            {
                context.Result = PolicyError(
                    $"scope '{s}' exceeds delegation ceiling for {parentType} -> {childType}");
                return;
            }
        }

        // Chain depth: the new chain is the subject_token's existing act chain plus this actor.
        var newDepth = CountActChainDepth(subject) + 1;
        if (policy.MaxDepth > 0 && newDepth > policy.MaxDepth)
        {
            context.Result = PolicyError(
                $"delegation chain depth {newDepth} exceeds max {policy.MaxDepth}");
            return;
        }

        // 3c. Chain-revocation enforcement (Milestone 3).
        //
        //     Every agent in the delegation chain must currently be active. The chain is the new
        //     actor (childAgentId, doing this exchange) plus every actor already named in the
        //     subject_token's act chain. If ANY of them is missing from the registry or not in
        //     status "active" (e.g. suspended/failed/completed), reject the exchange — this stops
        //     any new or refreshed delegation routed through a revoked agent, whether it is the
        //     immediate delegator or an ancestor.
        var chainAgentIds = new HashSet<string> { childAgentId };
        foreach (var id in AllActChainAgentIds(subject))
        {
            chainAgentIds.Add(id);
        }
        foreach (var id in chainAgentIds)
        {
            var agent = await _registry.GetAgent(id);
            if (agent is null || agent.Status != "active")
            {
                context.Result = PolicyError(
                    $"delegation chain member {id} is not active (status: {agent?.Status ?? "unknown"})");
                return;
            }
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

    /// <summary>Rejects a delegation-policy violation with an invalid_grant error.</summary>
    private static GrantValidationResult PolicyError(string description) =>
        new GrantValidationResult(TokenRequestErrors.InvalidGrant, description);

    /// <summary>
    /// The agentId is the last path segment of a SPIFFE URI
    /// (spiffe://cluster.local/agent/&lt;tenant&gt;/&lt;user&gt;/&lt;agentType&gt;/&lt;agentId&gt;).
    /// </summary>
    private static string? LastPathSegment(string? spiffe)
    {
        if (string.IsNullOrEmpty(spiffe)) return null;
        var seg = spiffe.Split('/', StringSplitOptions.RemoveEmptyEntries).LastOrDefault();
        return string.IsNullOrEmpty(seg) ? null : seg;
    }

    /// <summary>
    /// Returns the outermost actor's "sub" from the subject_token's act claim — i.e. the
    /// immediate delegator. The act claim is a nested object { "sub": "...", "act": { ... } };
    /// the top-level "sub" is the most recent actor. Returns null if there is no act claim,
    /// it is not parseable, or it has no top-level "sub".
    /// </summary>
    private static string? OutermostActSub(JsonWebToken subject)
    {
        if (!subject.TryGetClaim("act", out var actClaim) || string.IsNullOrEmpty(actClaim.Value))
            return null;
        try
        {
            using var doc = JsonDocument.Parse(actClaim.Value);
            if (doc.RootElement.ValueKind == JsonValueKind.Object &&
                doc.RootElement.TryGetProperty("sub", out var subProp) &&
                subProp.ValueKind == JsonValueKind.String)
            {
                return subProp.GetString();
            }
        }
        catch { /* malformed act → treated as unresolvable below */ }
        return null;
    }

    /// <summary>
    /// Returns the agentId of every actor named in the subject_token's act chain, by walking the
    /// nested act objects and mapping each level's "sub" (a SPIFFE URI) to its agentId via
    /// <see cref="LastPathSegment"/>. Levels without a usable "sub" are skipped. A subject_token
    /// with no act claim yields an empty sequence.
    /// </summary>
    private static IEnumerable<string> AllActChainAgentIds(JsonWebToken subject)
    {
        if (!subject.TryGetClaim("act", out var actClaim) || string.IsNullOrEmpty(actClaim.Value))
            yield break;

        JsonDocument doc;
        try
        {
            doc = JsonDocument.Parse(actClaim.Value);
        }
        catch
        {
            yield break;
        }

        using (doc)
        {
            var node = doc.RootElement;
            while (node.ValueKind == JsonValueKind.Object)
            {
                if (node.TryGetProperty("sub", out var subProp) &&
                    subProp.ValueKind == JsonValueKind.String)
                {
                    var agentId = LastPathSegment(subProp.GetString());
                    if (!string.IsNullOrEmpty(agentId))
                        yield return agentId;
                }
                if (!node.TryGetProperty("act", out var nested)) break;
                node = nested;
            }
        }
    }

    /// <summary>
    /// Counts the number of actors already present in the subject_token's act chain by walking
    /// the nested act objects: the outermost act counts as 1, plus 1 for each nested "act".
    /// A subject_token with no act claim has a chain depth of 0.
    ///
    /// Assumed shape: act = { "sub": "...", "act": { "sub": "...", "act": { ... } } } — a
    /// single linearly-nested chain. Each level is counted once regardless of whether it has a
    /// "sub" (depth tracks the structural chain length built by <see cref="BuildActClaim"/>).
    /// </summary>
    private static int CountActChainDepth(JsonWebToken subject)
    {
        if (!subject.TryGetClaim("act", out var actClaim) || string.IsNullOrEmpty(actClaim.Value))
            return 0;
        try
        {
            using var doc = JsonDocument.Parse(actClaim.Value);
            var node = doc.RootElement;
            var depth = 0;
            while (node.ValueKind == JsonValueKind.Object)
            {
                depth++;
                if (!node.TryGetProperty("act", out var nested)) break;
                node = nested;
            }
            return depth;
        }
        catch { return 0; }
    }
}
