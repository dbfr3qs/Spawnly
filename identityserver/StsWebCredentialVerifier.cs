using System.Text.Json;
using Microsoft.IdentityModel.JsonWebTokens;
using Microsoft.IdentityModel.Tokens;

namespace IdentityServer;

/// <summary>Configuration for <see cref="StsWebCredentialVerifier"/>.</summary>
public record StsWebOptions(
    string JwksUrl, string Issuer, string Audience,
    string Namespace, string ServiceAccount, string ClusterArn, string PodSuffix);

/// <summary>
/// AWS STS web-identity attestor (outbound web identity federation). Validates a
/// <c>sts:GetWebIdentityToken</c> JWT against the account STS issuer's JWKS and
/// derives the agent id from the EKS-set, cluster-attested
/// <c>principal_tags.kubernetes-pod-name</c> — NOT from caller-supplied
/// <c>request_tags</c>. Mirror of Go <c>registrant.StsWebVerifier</c>; keep the
/// AgentId derivation in lock step (the consistency invariant).
/// </summary>
public class StsWebCredentialVerifier : IAgentCredentialVerifier
{
    private const string StsNamespaceClaim = "https://sts.amazonaws.com/";

    // JWKS rarely rotates and tokens live ≤1h, so an hour-long cache keeps the
    // STS JWKS endpoint off the per-token-mint hot path. A signature failure on
    // cached keys triggers a single forced refresh (see Verify) to ride out a
    // mid-cache key rotation.
    private static readonly TimeSpan CacheTtl = TimeSpan.FromHours(1);

    private readonly IHttpClientFactory _httpFactory;
    private readonly StsWebOptions _o;
    private readonly string _podSuffix;
    private readonly ILogger<StsWebCredentialVerifier> _log;

    private readonly SemaphoreSlim _refreshLock = new(1, 1);
    private JsonWebKeySet? _cachedKeys;
    private DateTimeOffset _cacheExpiry;

    public StsWebCredentialVerifier(
        IHttpClientFactory httpFactory, StsWebOptions options, ILogger<StsWebCredentialVerifier> log)
    {
        _httpFactory = httpFactory;
        _o = options;
        _log = log;
        _podSuffix = string.IsNullOrEmpty(options.PodSuffix) ? "-pod" : options.PodSuffix;
        if (string.IsNullOrEmpty(options.ClusterArn))
            _log.LogWarning(
                "STSWEB_CLUSTER_ARN unset — accepting aws-stsweb tokens from any EKS cluster in the account");
    }

    public async Task<AgentIdentity?> Verify(string credential)
    {
        if (string.IsNullOrEmpty(credential)) return null;
        try
        {
            var handler = new JsonWebTokenHandler();
            var tvp = new TokenValidationParameters
            {
                ValidateIssuerSigningKey = true,
                ValidateLifetime = true,
                ValidateAudience = !string.IsNullOrEmpty(_o.Audience),
                ValidAudience = _o.Audience,
                ValidateIssuer = !string.IsNullOrEmpty(_o.Issuer),
                ValidIssuer = _o.Issuer,
            };

            var (keys, fromCache) = await GetSigningKeysAsync(forceRefresh: false);
            tvp.IssuerSigningKeys = keys;
            var result = await handler.ValidateTokenAsync(credential, tvp);
            // Cached keys can go stale across a rotation; refetch once and retry.
            // Freshly-fetched keys that still fail mean a bad token — don't refetch
            // (that would let garbage tokens amplify into JWKS requests).
            if (!result.IsValid && fromCache)
            {
                (tvp.IssuerSigningKeys, _) = await GetSigningKeysAsync(forceRefresh: true);
                result = await handler.ValidateTokenAsync(credential, tvp);
            }
            if (!result.IsValid)
            {
                _log.LogDebug("aws-stsweb token failed validation: {Error}", result.Exception?.Message);
                return null;
            }

            var jwt = handler.ReadJsonWebToken(credential);
            if (!jwt.TryGetPayloadValue<JsonElement>(StsNamespaceClaim, out var ns))
            {
                _log.LogDebug("aws-stsweb token missing {Claim} object", StsNamespaceClaim);
                return null;
            }
            if (!ns.TryGetProperty("principal_tags", out var tags))
            {
                _log.LogDebug("aws-stsweb token missing principal_tags (pod not under EKS Pod Identity?)");
                return null;
            }

            string? Tag(string k) => tags.TryGetProperty(k, out var v) ? v.GetString() : null;

            // Defense in depth: assert the EKS-attested context matches expectations.
            if (!Matches(Tag("kubernetes-namespace"), _o.Namespace)
                || !Matches(Tag("kubernetes-service-account"), _o.ServiceAccount)
                || !Matches(Tag("eks-cluster-arn"), _o.ClusterArn))
            {
                _log.LogDebug("aws-stsweb attested context did not match expected namespace/service-account/cluster");
                return null;
            }

            var podName = Tag("kubernetes-pod-name");
            if (string.IsNullOrEmpty(podName))
            {
                _log.LogDebug("aws-stsweb token missing principal_tags.kubernetes-pod-name");
                return null;
            }

            var agentId = podName.EndsWith(_podSuffix, StringComparison.Ordinal)
                ? podName[..^_podSuffix.Length]
                : podName;
            if (string.IsNullOrEmpty(agentId))
            {
                _log.LogDebug("aws-stsweb pod name {Pod} yields empty agentId", podName);
                return null;
            }

            // Subject is path-style so the token-exchange act-chain handling
            // (last path segment) recovers the agentId, matching SPIRE's shape.
            // Empty-or-missing eks-cluster-arn falls back to "eks" — kept in lock
            // step with Go registrant.identityFromTags (the consistency invariant).
            var arn = Tag("eks-cluster-arn");
            var clusterArn = string.IsNullOrEmpty(arn) ? "eks" : arn;
            return new AgentIdentity(agentId, $"{clusterArn}/agent/{agentId}", "aws-stsweb");
        }
        catch (Exception ex)
        {
            _log.LogWarning(ex, "aws-stsweb verification failed");
            return null;
        }
    }

    // GetSigningKeysAsync returns the STS issuer's signing keys, served from an
    // hour-long cache. FromCache reports whether the keys came from cache so the
    // caller can decide whether a validation failure is worth a forced refresh.
    private async Task<(IEnumerable<SecurityKey> Keys, bool FromCache)> GetSigningKeysAsync(bool forceRefresh)
    {
        if (!forceRefresh && _cachedKeys is not null && DateTimeOffset.UtcNow < _cacheExpiry)
            return (_cachedKeys.GetSigningKeys(), true);

        await _refreshLock.WaitAsync();
        try
        {
            if (!forceRefresh && _cachedKeys is not null && DateTimeOffset.UtcNow < _cacheExpiry)
                return (_cachedKeys.GetSigningKeys(), true);

            var http = _httpFactory.CreateClient();
            _cachedKeys = new JsonWebKeySet(await http.GetStringAsync(_o.JwksUrl));
            _cacheExpiry = DateTimeOffset.UtcNow + CacheTtl;
            return (_cachedKeys.GetSigningKeys(), false);
        }
        finally
        {
            _refreshLock.Release();
        }
    }

    private static bool Matches(string? got, string? want) =>
        string.IsNullOrEmpty(want) || got == want;
}
