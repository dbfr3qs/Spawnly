using Duende.IdentityServer;
using Duende.IdentityServer.Models;
using Duende.IdentityServer.Services;
using System.Security.Claims;

namespace IdentityServer;

/// <summary>
/// The one place a pending CIBA request is completed. Approval consents to all
/// requested scopes and — when the request carries a resolved spawn edge —
/// records the grant in the registry so the next spawn of the same edge
/// auto-approves. Denial completes with no consented scopes (Duende requires a
/// Subject either way; the poll then returns access_denied).
/// </summary>
public class CibaCompletionService
{
    private readonly IBackchannelAuthenticationInteractionService _interaction;
    private readonly AgentRegistryClient _registry;
    private readonly ILogger<CibaCompletionService> _log;

    public CibaCompletionService(
        IBackchannelAuthenticationInteractionService interaction,
        AgentRegistryClient registry,
        ILogger<CibaCompletionService> log)
    {
        _interaction = interaction;
        _registry = registry;
        _log = log;
    }

    /// <summary>Builds the principal a completion is signed with.</summary>
    public static ClaimsPrincipal SubjectFor(string userId) =>
        new IdentityServerUser(userId)
        {
            AuthenticationTime = DateTime.UtcNow,
            IdentityProvider = IdentityServerConstants.LocalIdentityProvider,
        }.CreatePrincipal();

    /// <summary>
    /// Approves a pending request. recordConsent=false skips the registry write
    /// (used by auto-approval, where the stored consent already covers the edge).
    /// </summary>
    public async Task<bool> ApproveAsync(
        BackchannelUserLoginRequest request, ClaimsPrincipal subject,
        string? sessionId = null, bool recordConsent = true)
    {
        var scopes = request.ValidatedResources.RawScopeValues;
        await _interaction.CompleteLoginRequestAsync(new CompleteBackchannelLoginRequest(request.InternalId)
        {
            Subject = subject,
            ScopesValuesConsented = scopes,
            SessionId = sessionId,
        });

        var userId = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.UserIdKey);
        var parentType = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.ParentTypeKey);
        var childType = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.ChildTypeKey);
        if (recordConsent &&
            userId is { Length: > 0 } && parentType is { Length: > 0 } && childType is { Length: > 0 })
        {
            if (!await _registry.RecordConsent(userId, parentType, childType, scopes))
            {
                // The approval stands (tokens will mint) but the next spawn of
                // this edge will prompt again instead of auto-approving.
                _log.LogWarning(
                    "consent approved but not recorded for {UserId} {ParentType}->{ChildType}",
                    userId, parentType, childType);
                return false;
            }
        }
        return true;
    }

    public Task DenyAsync(BackchannelUserLoginRequest request, ClaimsPrincipal subject) =>
        _interaction.CompleteLoginRequestAsync(
            new CompleteBackchannelLoginRequest(request.InternalId) { Subject = subject });
}
