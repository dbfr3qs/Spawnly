using Duende.IdentityServer.Validation;
using Microsoft.IdentityModel.JsonWebTokens; // JsonWebTokenHandler, JsonClaimValueTypes
using System.Security.Claims;
using System.Text.Json;

namespace IdentityServer;

/// <summary>
/// Runs for every token request. For the client_credentials path it sets the root
/// token identity: sub = "user:&lt;userId&gt;" and act = { "sub": "&lt;spiffe URI&gt;" }
/// (the agent is the actor delegating on behalf of the user).
///
/// It deliberately does NOT touch the token-exchange path — the
/// <see cref="TokenExchangeGrantValidator"/> sets its own sub/act there.
/// </summary>
public class AgentRegistryValidator : ICustomTokenRequestValidator
{
    private readonly AgentRegistryClient _registry;

    public AgentRegistryValidator(AgentRegistryClient registry) => _registry = registry;

    public async Task ValidateAsync(CustomTokenRequestValidationContext context)
    {
        var request = context.Result.ValidatedRequest;

        // CIBA poll: sub is the human (set by the grant itself); add the agent
        // actor chain from the SVID so resource servers can authorize the agent,
        // mirroring the client_credentials act shape. The poll must also pass
        // the same registry liveness check as client_credentials — an agent
        // revoked while its approved auth_req_id is outstanding may not redeem
        // it (real-time revocation has no CIBA-shaped gap).
        if (request.GrantType == Config.CibaGrantType)
        {
            var cibaAssertion = SpireClientSecretValidator.ValidatedAssertion(request.Raw);
            if (cibaAssertion is not null)
            {
                var cibaSpiffeId = new JsonWebTokenHandler().ReadJsonWebToken(cibaAssertion).Subject;
                var cibaAgentId = cibaSpiffeId.Split('/').Last();
                var cibaAgent = await _registry.GetAgent(cibaAgentId);
                if (cibaAgent?.Status is not ("active" or "pending" or "awaiting-consent"))
                {
                    Reject(context, $"agent {cibaAgentId} is {cibaAgent?.Status ?? "not registered"}");
                    return;
                }
                var cibaActJson = JsonSerializer.Serialize(
                    new Dictionary<string, string> { ["sub"] = cibaSpiffeId });
                request.ClientClaims?.Add(new Claim("act", cibaActJson, JsonClaimValueTypes.Json));
            }
            return;
        }

        // Only handle the client_credentials path. The token-exchange grant produces
        // its own sub/act via the extension grant validator.
        if (request.GrantType != "client_credentials")
        {
            return;
        }

        var assertion = SpireClientSecretValidator.ValidatedAssertion(request.Raw);
        if (assertion is null) { Reject(context, "missing client_assertion"); return; }

        var handler = new JsonWebTokenHandler();
        var jwt = handler.ReadJsonWebToken(assertion);
        var spiffeId = jwt.Subject;
        var agentId = spiffeId.Split('/').Last();

        var agent = await _registry.GetAgent(agentId);
        if (agent is null || agent.Status != "active")
        {
            Reject(context, $"agent {agentId} is not active");
            return;
        }

        if (string.IsNullOrEmpty(agent.UserId))
        {
            Reject(context, $"agent {agentId} has no userId");
            return;
        }

        var claims = request.ClientClaims;
        if (claims is null) { Reject(context, "client claims unavailable"); return; }

        // sub = user:<userId> — the human principal the agent acts for.
        claims.Add(new Claim("sub", $"user:{agent.UserId}"));

        // act = { "sub": "<spiffe URI>" } — the agent is the actor.
        // Must serialize as a nested JSON object, so tag the claim value type as JSON.
        var actJson = JsonSerializer.Serialize(new Dictionary<string, string> { ["sub"] = spiffeId });
        claims.Add(new Claim("act", actJson, JsonClaimValueTypes.Json));

        // audience=delegation → a delegation-only token a parent hands to a child
        // as the subject_token of a later exchange. Duende filters the reserved
        // `aud` claim, so token_use is the authoritative signal: resource servers
        // reject token_use=delegation; only the exchange grant accepts it.
        if (request.Raw?.Get("audience") == "delegation")
        {
            claims.Add(new Claim("token_use", "delegation"));
        }
    }

    private static void Reject(CustomTokenRequestValidationContext ctx, string desc)
    {
        ctx.Result.IsError = true;
        ctx.Result.Error = "invalid_client";
        ctx.Result.ErrorDescription = desc;
    }
}
