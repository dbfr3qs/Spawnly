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

    // Registry JSON uses camelCase fields: agentId, status, userId, agentType, parentId.
    public record AgentRecord(
        [property: JsonPropertyName("agentId")] string AgentId,
        [property: JsonPropertyName("status")] string Status,
        [property: JsonPropertyName("userId")] string? UserId,
        [property: JsonPropertyName("agentType")] string? AgentType,
        [property: JsonPropertyName("parentId")] string? ParentId);
}
