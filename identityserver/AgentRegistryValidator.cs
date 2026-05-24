using Duende.IdentityServer.Validation;
using Microsoft.IdentityModel.JsonWebTokens;
using System.Security.Claims;

namespace IdentityServer;

public class AgentRegistryValidator : ICustomTokenRequestValidator
{
    private readonly AgentRegistryClient _registry;

    public AgentRegistryValidator(AgentRegistryClient registry) => _registry = registry;

    public async Task ValidateAsync(CustomTokenRequestValidationContext context)
    {
        var assertion = context.Result.ValidatedRequest.ClientAssertion?.Value;
        if (assertion is null) { Reject(context, "missing client_assertion"); return; }

        var handler = new JsonWebTokenHandler();
        var jwt = handler.ReadJsonWebToken(assertion);
        var spiffeId = jwt.Subject;
        var agentId = spiffeId.Split('/').Last();

        if (!await _registry.IsActive(agentId))
        {
            Reject(context, $"agent {agentId} is not active");
            return;
        }

        context.Result.ValidatedRequest.ClientClaims.Add(new Claim("spiffe_id", spiffeId));
    }

    private static void Reject(CustomTokenRequestValidationContext ctx, string desc)
    {
        ctx.Result.IsError = true;
        ctx.Result.Error = "invalid_client";
        ctx.Result.ErrorDescription = desc;
    }
}
