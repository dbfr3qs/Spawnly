using Duende.IdentityServer.Validation;
using Microsoft.IdentityModel.JsonWebTokens;

namespace IdentityServer;

/// <summary>
/// Final validation step for a CIBA backchannel authentication request: binds
/// the request to a real spawn edge. The requesting client is a child agent
/// authenticating with its JWT-SVID, so the agent instance, its user, and its
/// parent are all derived from the registry record behind that SVID — nothing
/// about the edge is trusted from request parameters. The resolved context is
/// stashed in the request Properties for the notification service (consent
/// auto-approval) and the consent API (what the user sees) to read.
/// </summary>
public class CibaRequestValidator : ICustomBackchannelAuthenticationValidator
{
    // Properties keys carried on the stored backchannel request.
    public const string AgentIdKey = "spawnly:agentId";
    public const string UserIdKey = "spawnly:userId";
    public const string ParentTypeKey = "spawnly:parentType";
    public const string ChildTypeKey = "spawnly:childType";

    /// <summary>
    /// Typed read of one of the keys above. Properties values are object: a
    /// plain string before the request is stored, but a JsonElement after a
    /// round-trip through the grant store — ToString() covers both.
    /// </summary>
    public static string? Property(IDictionary<string, object> properties, string key) =>
        properties.TryGetValue(key, out var v) ? v?.ToString() : null;

    private readonly AgentRegistryClient _registry;
    private readonly ILogger<CibaRequestValidator> _log;

    public CibaRequestValidator(AgentRegistryClient registry, ILogger<CibaRequestValidator> log)
    {
        _registry = registry;
        _log = log;
    }

    public async Task ValidateAsync(CustomBackchannelAuthenticationRequestValidationContext context)
    {
        var result = context.ValidationResult;
        if (result.IsError) return;
        var request = result.ValidatedRequest;

        // The user the request asks to authenticate, resolved by CibaUserValidator.
        var hintedUser = request.Subject?.FindFirst("sub")?.Value;

        // Only an assertion that client authentication signature-checked
        // against SPIRE may anchor the edge; a raw form read would let a
        // secret-authenticated caller smuggle in a forged SVID.
        var assertion = AgentClientSecretValidator.ValidatedAssertion(request.Raw);
        if (assertion is null)
        {
            // Non-SVID client authentication only exists off-cluster (local dev
            // drives CIBA with the placeholder client_secret). There is no agent
            // record to resolve the edge from, so accept the edge from request
            // parameters — but only when the dev API is explicitly enabled.
            if (Environment.GetEnvironmentVariable("DEV_CIBA_API") != "true")
            {
                Reject(context, "client_assertion required for CIBA");
                return;
            }
            request.Properties[UserIdKey] = hintedUser ?? "";
            request.Properties[ParentTypeKey] = request.Raw?.Get("parent_type") ?? "";
            request.Properties[ChildTypeKey] = request.ClientId ?? "";
            return;
        }

        var spiffeId = new JsonWebTokenHandler().ReadJsonWebToken(assertion).Subject;
        var agentId = spiffeId.Split('/').Last();

        var agent = await _registry.GetAgent(agentId);
        if (agent is null)
        {
            Reject(context, $"agent {agentId} not registered");
            return;
        }
        // pending/awaiting-consent: the sidecar runs CIBA before the agent is
        // fully up. Revoked/terminal agents may not open consent requests.
        if (agent.Status is not ("active" or "pending" or "awaiting-consent"))
        {
            Reject(context, $"agent {agentId} is {agent.Status}");
            return;
        }
        if (string.IsNullOrEmpty(agent.UserId))
        {
            Reject(context, $"agent {agentId} has no user to ask for consent");
            return;
        }
        if (hintedUser != agent.UserId)
        {
            Reject(context, "login_hint does not match the agent's user");
            return;
        }
        if (string.IsNullOrEmpty(agent.ParentId))
        {
            // Consent gates a parent->child spawn edge; a parentless agent has
            // no edge to consent to (root spawns are the user's own action).
            Reject(context, $"agent {agentId} has no parent; consent applies to spawned children");
            return;
        }
        var parent = await _registry.GetAgent(agent.ParentId);
        if (parent?.AgentType is null)
        {
            Reject(context, $"parent {agent.ParentId} of agent {agentId} is unknown");
            return;
        }

        request.Properties[AgentIdKey] = agentId;
        request.Properties[UserIdKey] = agent.UserId;
        request.Properties[ParentTypeKey] = parent.AgentType;
        request.Properties[ChildTypeKey] = agent.AgentType ?? request.ClientId ?? "";
        _log.LogInformation(
            "CIBA request from agent {AgentId}: user={UserId} edge={ParentType}->{ChildType}",
            agentId, agent.UserId, parent.AgentType, agent.AgentType);
    }

    private static void Reject(
        CustomBackchannelAuthenticationRequestValidationContext context, string description)
    {
        context.ValidationResult = new BackchannelAuthenticationRequestValidationResult(
            context.ValidationResult.ValidatedRequest, "invalid_request", description);
    }
}
