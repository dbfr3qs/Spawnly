using System.Text.Json.Serialization;

namespace IdentityServer;

public class AgentRegistryClient
{
    private readonly string _baseUrl;
    private readonly HttpClient _http = new();

    public AgentRegistryClient(string baseUrl) => _baseUrl = baseUrl;

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
