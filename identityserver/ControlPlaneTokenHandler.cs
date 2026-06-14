using System.Net.Http.Headers;
using System.Text.Json;

namespace IdentityServer;

/// <summary>
/// DelegatingHandler that attaches a client-credentials access token to every
/// outbound request — used by <see cref="AgentRegistryClient"/> to call the
/// registry's consent endpoints when the registry runs with
/// CONTROL_PLANE_AUTH=oidc. The token is fetched from the configured token
/// endpoint and cached until shortly before expiry; concurrent callers share a
/// single in-flight fetch.
///
/// This is the IdP-side counterpart of the orchestrator's oauth2 TokenSource.
/// It is inert in the local demo, where CONTROL_PLANE_AUTH is unset.
/// </summary>
public class ControlPlaneTokenHandler : DelegatingHandler
{
    private readonly string _tokenUrl;
    private readonly string _clientId;
    private readonly string _clientSecret;
    private readonly string _scope;
    private readonly HttpClient _tokenHttp = new();
    private readonly SemaphoreSlim _lock = new(1, 1);

    private string? _token;
    private DateTimeOffset _expiresAt = DateTimeOffset.MinValue;

    public ControlPlaneTokenHandler(string tokenUrl, string clientId, string clientSecret, string scope)
        : base(new HttpClientHandler())
    {
        _tokenUrl = tokenUrl;
        _clientId = clientId;
        _clientSecret = clientSecret;
        _scope = scope;
    }

    protected override async Task<HttpResponseMessage> SendAsync(
        HttpRequestMessage request, CancellationToken ct)
    {
        var token = await GetTokenAsync(ct);
        if (token is not null)
        {
            request.Headers.Authorization = new AuthenticationHeaderValue("Bearer", token);
        }
        return await base.SendAsync(request, ct);
    }

    private async Task<string?> GetTokenAsync(CancellationToken ct)
    {
        if (_token is not null && DateTimeOffset.UtcNow < _expiresAt)
        {
            return _token;
        }

        await _lock.WaitAsync(ct);
        try
        {
            // Re-check: another caller may have refreshed while we waited.
            if (_token is not null && DateTimeOffset.UtcNow < _expiresAt)
            {
                return _token;
            }

            var form = new FormUrlEncodedContent(new Dictionary<string, string>
            {
                ["grant_type"] = "client_credentials",
                ["client_id"] = _clientId,
                ["client_secret"] = _clientSecret,
                ["scope"] = _scope,
            });
            using var resp = await _tokenHttp.PostAsync(_tokenUrl, form, ct);
            resp.EnsureSuccessStatusCode();
            await using var stream = await resp.Content.ReadAsStreamAsync(ct);
            using var doc = await JsonDocument.ParseAsync(stream, cancellationToken: ct);
            var root = doc.RootElement;

            _token = root.GetProperty("access_token").GetString();
            var expiresIn = root.TryGetProperty("expires_in", out var e) ? e.GetInt32() : 3600;
            // Refresh ~60s early so an in-flight request never carries a just-expired token.
            _expiresAt = DateTimeOffset.UtcNow.AddSeconds(Math.Max(expiresIn - 60, 30));
            return _token;
        }
        finally
        {
            _lock.Release();
        }
    }
}
