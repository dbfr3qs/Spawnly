using System.Text.Json.Serialization;

namespace IdentityServer;

public class AgentRegistryClient
{
    private readonly string _baseUrl;
    private readonly HttpClient _http;

    public AgentRegistryClient(string baseUrl)
    {
        _baseUrl = baseUrl;
        // The IdP's CIBA driver is a trusted control-plane caller of the
        // registry's consent endpoints. CONTROL_PLANE_AUTH selects how it
        // authenticates, matching the registry's setting:
        //   none/unset    -> no header (local demo; registry enforces nothing)
        //   shared-secret -> static CONTROL_PLANE_TOKEN bearer
        //   oidc          -> client-credentials token, fetched + cached by the
        //                    ControlPlaneTokenHandler (the "idp-consent" client)
        switch (Environment.GetEnvironmentVariable("CONTROL_PLANE_AUTH"))
        {
            case "oidc":
                var scope = Environment.GetEnvironmentVariable("CONTROL_PLANE_SCOPE") ?? "registry.consent";
                _http = new HttpClient(new ControlPlaneTokenHandler(
                    Environment.GetEnvironmentVariable("CONTROL_PLANE_TOKEN_URL") ?? "",
                    Environment.GetEnvironmentVariable("CONTROL_PLANE_CLIENT_ID") ?? "",
                    Environment.GetEnvironmentVariable("CONTROL_PLANE_CLIENT_SECRET") ?? "",
                    scope));
                break;
            case "shared-secret":
                _http = new HttpClient();
                var token = Environment.GetEnvironmentVariable("CONTROL_PLANE_TOKEN");
                if (!string.IsNullOrEmpty(token))
                {
                    _http.DefaultRequestHeaders.Authorization =
                        new System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", token);
                }
                break;
            default:
                _http = new HttpClient();
                break;
        }
    }

    public async Task<bool> IsActive(string agentId)
    {
        var agent = await GetAgent(agentId);
        return agent?.Status == "active";
    }

    /// <summary>
    /// Fetches the full agent record from the registry.
    /// Returns null if the agent does not exist or the registry is unreachable.
    /// </summary>
    public async Task<AgentRecord?> GetAgent(string agentId)
    {
        try
        {
            var resp = await _http.GetAsync($"{_baseUrl}/v1/agents/{agentId}");
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<AgentRecord>();
        }
        catch { return null; }
    }

    /// <summary>
    /// Fetches the delegation policy for a (parentType -> childType) edge.
    /// Returns null only if the registry is unreachable or returns a non-success status;
    /// a well-formed 200 response is returned as-is (which may carry Allowed == false when
    /// the edge is not permitted or no policy exists).
    /// </summary>
    public async Task<DelegationPolicy?> GetDelegationPolicy(string parentType, string childType)
    {
        try
        {
            var url = $"{_baseUrl}/v1/delegation-policy" +
                      $"?parentType={Uri.EscapeDataString(parentType)}" +
                      $"&childType={Uri.EscapeDataString(childType)}";
            var resp = await _http.GetAsync(url);
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<DelegationPolicy>();
        }
        catch { return null; }
    }

    /// <summary>
    /// Asks the registry whether a stored consent covers a spawn edge with the
    /// requested scopes right now. Null only when the registry is unreachable;
    /// callers must treat that as "not granted" (prompt the user).
    /// </summary>
    public async Task<ConsentDecision?> CheckConsent(
        string userId, string parentType, string childType, IEnumerable<string> scopes)
    {
        try
        {
            var url = $"{_baseUrl}/v1/consents/check" +
                      $"?userId={Uri.EscapeDataString(userId)}" +
                      $"&parentType={Uri.EscapeDataString(parentType)}" +
                      $"&childType={Uri.EscapeDataString(childType)}" +
                      string.Concat(scopes.Select(s => $"&scope={Uri.EscapeDataString(s)}"));
            var resp = await _http.GetAsync(url);
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<ConsentDecision>();
        }
        catch { return null; }
    }

    /// <summary>
    /// Records a fresh user grant for a spawn edge (called when a CIBA request
    /// is approved). The registry derives the expiry from the parent template's
    /// consentTTL and replaces any prior record for the edge.
    /// </summary>
    public async Task<bool> RecordConsent(
        string userId, string parentType, string childType, IEnumerable<string> scopes)
    {
        try
        {
            var resp = await _http.PostAsJsonAsync($"{_baseUrl}/v1/consents", new
            {
                userId,
                parentType,
                childType,
                scopes = scopes.ToArray(),
            });
            return resp.IsSuccessStatusCode;
        }
        catch { return false; }
    }

    /// <summary>
    /// Creates (or fetches the existing open) pending consent request for a
    /// spawn edge. Returns the ConsentRequest as-is — its Status may already
    /// be "approved" if the registry short-circuited on a covering stored
    /// consent. Null only when the registry is unreachable or errors.
    /// </summary>
    public async Task<ConsentRequest?> CreateConsentRequest(
        string userId, string parentType, string childType, IEnumerable<string> scopes,
        string? bindingMessage, string? externalRef, string? agentId = null)
    {
        try
        {
            var resp = await _http.PostAsJsonAsync($"{_baseUrl}/v1/consent-requests", new
            {
                userId,
                parentType,
                childType,
                agentId,
                scopes = scopes.ToArray(),
                bindingMessage,
                externalRef,
            });
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<ConsentRequest>();
        }
        catch { return null; }
    }

    /// <summary>
    /// Fetches a single consent request by id (used by the completion poller to
    /// learn the registry's decision). Null when not found or unreachable.
    /// </summary>
    public async Task<ConsentRequest?> GetConsentRequest(string id)
    {
        try
        {
            var resp = await _http.GetAsync($"{_baseUrl}/v1/consent-requests/{id}");
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<ConsentRequest>();
        }
        catch { return null; }
    }

    /// <summary>
    /// Approves a pending consent request, recording the corresponding
    /// ConsentRecord in the registry and sweeping any other open requests for
    /// the same edge. Null only when the registry is unreachable or errors.
    /// </summary>
    public async Task<ConsentRequest?> ApproveConsentRequest(string id, IEnumerable<string>? scopes)
    {
        try
        {
            var resp = await _http.PostAsJsonAsync($"{_baseUrl}/v1/consent-requests/{id}/approve", new
            {
                scopes = scopes?.ToArray(),
            });
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<ConsentRequest>();
        }
        catch { return null; }
    }

    /// <summary>
    /// Denies a pending consent request. Null only when the registry is
    /// unreachable or errors.
    /// </summary>
    public async Task<ConsentRequest?> DenyConsentRequest(string id)
    {
        try
        {
            var resp = await _http.PostAsync($"{_baseUrl}/v1/consent-requests/{id}/deny", null);
            if (!resp.IsSuccessStatusCode) return null;
            return await resp.Content.ReadFromJsonAsync<ConsentRequest>();
        }
        catch { return null; }
    }

    public record ConsentDecision(
        [property: JsonPropertyName("granted")] bool Granted,
        [property: JsonPropertyName("reason")] string? Reason);

    // Registry JSON uses camelCase fields: id, userId, parentType, childType,
    // agentId, scopes, bindingMessage, status, createdAt, resolvedAt, externalRef.
    public record ConsentRequest(
        [property: JsonPropertyName("id")] string Id,
        [property: JsonPropertyName("userId")] string UserId,
        [property: JsonPropertyName("parentType")] string ParentType,
        [property: JsonPropertyName("childType")] string ChildType,
        [property: JsonPropertyName("agentId")] string? AgentId,
        [property: JsonPropertyName("scopes")] List<string>? Scopes,
        [property: JsonPropertyName("bindingMessage")] string? BindingMessage,
        [property: JsonPropertyName("status")] string Status,
        [property: JsonPropertyName("createdAt")] DateTime CreatedAt,
        [property: JsonPropertyName("resolvedAt")] DateTime? ResolvedAt,
        [property: JsonPropertyName("externalRef")] string? ExternalRef);

    // Registry JSON uses camelCase fields: agentId, status, userId, agentType, parentId.
    public record AgentRecord(
        [property: JsonPropertyName("agentId")] string AgentId,
        [property: JsonPropertyName("status")] string Status,
        [property: JsonPropertyName("userId")] string? UserId,
        [property: JsonPropertyName("agentType")] string? AgentType,
        [property: JsonPropertyName("parentId")] string? ParentId);

    // GET /v1/delegation-policy?parentType=X&childType=Y →
    //   { "allowed": bool, "grantableScopes": ["..."], "maxDepth": int }
    public record DelegationPolicy(
        [property: JsonPropertyName("allowed")] bool Allowed,
        [property: JsonPropertyName("grantableScopes")] List<string>? GrantableScopes,
        [property: JsonPropertyName("maxDepth")] int MaxDepth);
}
