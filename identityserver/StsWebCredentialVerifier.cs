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

    private readonly IHttpClientFactory _httpFactory;
    private readonly StsWebOptions _o;
    private readonly string _podSuffix;

    public StsWebCredentialVerifier(IHttpClientFactory httpFactory, StsWebOptions options)
    {
        _httpFactory = httpFactory;
        _o = options;
        _podSuffix = string.IsNullOrEmpty(options.PodSuffix) ? "-pod" : options.PodSuffix;
    }

    public async Task<AgentIdentity?> Verify(string credential)
    {
        if (string.IsNullOrEmpty(credential)) return null;
        try
        {
            var http = _httpFactory.CreateClient();
            var jwks = new JsonWebKeySet(await http.GetStringAsync(_o.JwksUrl));

            var handler = new JsonWebTokenHandler();
            var result = await handler.ValidateTokenAsync(credential, new TokenValidationParameters
            {
                IssuerSigningKeys = jwks.GetSigningKeys(),
                ValidateIssuerSigningKey = true,
                ValidateLifetime = true,
                ValidateAudience = !string.IsNullOrEmpty(_o.Audience),
                ValidAudience = _o.Audience,
                ValidateIssuer = !string.IsNullOrEmpty(_o.Issuer),
                ValidIssuer = _o.Issuer,
            });
            if (!result.IsValid) return null;

            var jwt = handler.ReadJsonWebToken(credential);
            if (!jwt.TryGetPayloadValue<JsonElement>(StsNamespaceClaim, out var ns)) return null;
            if (!ns.TryGetProperty("principal_tags", out var tags)) return null;

            string? Tag(string k) => tags.TryGetProperty(k, out var v) ? v.GetString() : null;

            // Defense in depth: assert the EKS-attested context matches expectations.
            if (!Matches(Tag("kubernetes-namespace"), _o.Namespace)) return null;
            if (!Matches(Tag("kubernetes-service-account"), _o.ServiceAccount)) return null;
            if (!Matches(Tag("eks-cluster-arn"), _o.ClusterArn)) return null;

            var podName = Tag("kubernetes-pod-name");
            if (string.IsNullOrEmpty(podName)) return null;

            var agentId = podName.EndsWith(_podSuffix, StringComparison.Ordinal)
                ? podName[..^_podSuffix.Length]
                : podName;
            if (string.IsNullOrEmpty(agentId)) return null;

            // Subject is path-style so the token-exchange act-chain handling
            // (last path segment) recovers the agentId, matching SPIRE's shape.
            var clusterArn = Tag("eks-cluster-arn") ?? "eks";
            return new AgentIdentity(agentId, $"{clusterArn}/agent/{agentId}", "aws-stsweb");
        }
        catch
        {
            return null;
        }
    }

    private static bool Matches(string? got, string? want) =>
        string.IsNullOrEmpty(want) || got == want;
}
