namespace IdentityServer;

public class AgentRegistryClient
{
    private readonly string _baseUrl;
    private readonly HttpClient _http = new();

    public AgentRegistryClient(string baseUrl) => _baseUrl = baseUrl;

    public async Task<bool> IsActive(string agentId)
    {
        try
        {
            var resp = await _http.GetAsync($"{_baseUrl}/v1/agents/{agentId}");
            if (!resp.IsSuccessStatusCode) return false;
            var body = await resp.Content.ReadFromJsonAsync<AgentRecord>();
            return body?.Status == "active";
        }
        catch { return false; }
    }

    private record AgentRecord(string AgentId, string Status);
}
