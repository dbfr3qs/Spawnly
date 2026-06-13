using Duende.IdentityServer;
using Duende.IdentityServer.Models;
using Duende.IdentityServer.Services;
using System.Collections.Concurrent;
using System.Security.Claims;

namespace IdentityServer;

/// <summary>
/// The one place a pending CIBA request is completed. Approval consents to all
/// requested scopes and — when the request carries a resolved spawn edge —
/// approves the corresponding registry consent request, which records the
/// grant and sweeps other pending requests for the same edge. Denial
/// completes with no consented scopes (Duende requires a Subject either way;
/// the poll then returns access_denied) and denies the registry request.
/// </summary>
public class CibaCompletionService
{
    private readonly IBackchannelAuthenticationInteractionService _interaction;
    private readonly AgentRegistryClient _registry;
    private readonly ILogger<CibaCompletionService> _log;

    // Maps a CIBA request's InternalId to the registry's ConsentRequest.ID,
    // populated by CibaConsentNotificationService when it creates the registry
    // request. Lets ApproveAsync/DenyAsync resolve which registry request to
    // resolve without threading the id through Duende's request Properties.
    // In-memory and best-effort: a process restart between request-creation
    // and approval loses the mapping (the registry request remains pending
    // and can still be resolved directly via the registry's own API/dashboard).
    private readonly ConcurrentDictionary<string, string> _registryRequestIds = new();

    public CibaCompletionService(
        IBackchannelAuthenticationInteractionService interaction,
        AgentRegistryClient registry,
        ILogger<CibaCompletionService> log)
    {
        _interaction = interaction;
        _registry = registry;
        _log = log;
    }

    /// <summary>
    /// Records the registry's ConsentRequest.ID for a CIBA request's
    /// InternalId, so a later ApproveAsync/DenyAsync can resolve it in the
    /// registry. Called by CibaConsentNotificationService after it creates the
    /// registry consent request.
    /// </summary>
    public void TrackConsentRequest(string internalId, string registryRequestId) =>
        _registryRequestIds[internalId] = registryRequestId;

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
            if (!_registryRequestIds.TryGetValue(request.InternalId, out var registryRequestId))
            {
                // No registry consent request was tracked for this CIBA request
                // (e.g. a dev/manual approval path that bypassed
                // SendLoginRequestAsync). The approval stands (tokens will
                // mint) but the registry's ConsentRecord is not updated, so
                // the next spawn of this edge will prompt again.
                _log.LogWarning(
                    "consent approved but no registry consent request tracked for {UserId} {ParentType}->{ChildType}",
                    userId, parentType, childType);
                return true;
            }
            if (await _registry.ApproveConsentRequest(registryRequestId, scopes) is null)
            {
                // The approval stands (tokens will mint) but the next spawn of
                // this edge will prompt again instead of auto-approving.
                _log.LogWarning(
                    "consent approved but registry consent request {Id} not approved for {UserId} {ParentType}->{ChildType}",
                    registryRequestId, userId, parentType, childType);
                return false;
            }
            _registryRequestIds.TryRemove(request.InternalId, out _);
        }
        return true;
    }

    public async Task DenyAsync(BackchannelUserLoginRequest request, ClaimsPrincipal subject)
    {
        await _interaction.CompleteLoginRequestAsync(
            new CompleteBackchannelLoginRequest(request.InternalId) { Subject = subject });

        if (_registryRequestIds.TryRemove(request.InternalId, out var registryRequestId))
        {
            await _registry.DenyConsentRequest(registryRequestId);
        }
    }
}
