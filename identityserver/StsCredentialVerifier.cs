using System.Text;
using System.Text.Json;
using System.Text.RegularExpressions;
using System.Xml.Linq;

namespace IdentityServer;

/// <summary>
/// AWS STS attestor. Verifies a presigned <c>GetCallerIdentity</c> credential by
/// replaying it against AWS STS — AWS is the attestor, since a valid SigV4
/// signature is required for STS to answer — then derives
/// <see cref="AgentIdentity.AgentId"/> from the assumed-role session name (the
/// operator sets the session name to the agentId).
///
/// Mirrors Go <c>internal/attestor/aws_verify.go</c>; keep the two in lock step
/// so the registry and IdentityServer derive the same AgentId (the consistency
/// invariant).
/// </summary>
public class StsCredentialVerifier : IAgentCredentialVerifier
{
    private static readonly Regex StsHost =
        new(@"^sts(\.[a-z0-9-]+)?\.amazonaws\.com$", RegexOptions.Compiled);

    private readonly IHttpClientFactory _httpFactory;

    public StsCredentialVerifier(IHttpClientFactory httpFactory) => _httpFactory = httpFactory;

    private record PresignedRequest(string? method, string? url, Dictionary<string, string[]>? headers);

    public async Task<AgentIdentity?> Verify(string credential)
    {
        PresignedRequest? pr;
        try
        {
            var json = Encoding.UTF8.GetString(Convert.FromBase64String(credential));
            pr = JsonSerializer.Deserialize<PresignedRequest>(json);
        }
        catch
        {
            return null;
        }
        if (pr is null || string.IsNullOrEmpty(pr.url) || string.IsNullOrEmpty(pr.method)) return null;
        if (!ValidateStsUrl(pr.url)) return null;

        try
        {
            var http = _httpFactory.CreateClient();
            using var req = new HttpRequestMessage(new HttpMethod(pr.method), pr.url);
            if (pr.headers is not null)
            {
                foreach (var (k, vs) in pr.headers)
                    foreach (var v in vs)
                        req.Headers.TryAddWithoutValidation(k, v);
            }

            var resp = await http.SendAsync(req);
            if (!resp.IsSuccessStatusCode) return null;

            var body = await resp.Content.ReadAsStringAsync();
            var arn = ParseArn(body);
            if (arn is null) return null;

            var sessionName = SessionNameFromArn(arn);
            if (sessionName is null) return null;

            return new AgentIdentity(sessionName, arn, "aws-sts");
        }
        catch
        {
            return null;
        }
    }

    // The verifier replays a caller-supplied URL, so it MUST confirm the URL
    // targets an STS endpoint over HTTPS and invokes GetCallerIdentity — else a
    // caller could point the control plane at an arbitrary URL (SSRF).
    private static bool ValidateStsUrl(string raw)
    {
        if (!Uri.TryCreate(raw, UriKind.Absolute, out var u)) return false;
        if (u.Scheme != "https") return false;
        if (!StsHost.IsMatch(u.Host)) return false;
        return raw.Contains("Action=GetCallerIdentity");
    }

    private static string? ParseArn(string xml)
    {
        try
        {
            // Match by local name to ignore the STS response XML namespace.
            return XDocument.Parse(xml).Descendants()
                .FirstOrDefault(e => e.Name.LocalName == "Arn")?.Value;
        }
        catch
        {
            return null;
        }
    }

    private static string? SessionNameFromArn(string arn)
    {
        // arn:aws:sts::<acct>:assumed-role/<role>/<sessionName>
        const string marker = ":assumed-role/";
        var i = arn.IndexOf(marker, StringComparison.Ordinal);
        if (i < 0) return null;
        var rest = arn[(i + marker.Length)..];
        var parts = rest.Split('/', 2);
        if (parts.Length != 2 || parts[1].Length == 0) return null;
        return parts[1];
    }
}
