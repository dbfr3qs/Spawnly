using Duende.IdentityServer;
using Duende.IdentityServer.Models;
using Duende.IdentityServer.Services;
using Duende.IdentityServer.Stores;
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
    private readonly IBackChannelAuthenticationRequestStore _store;
    private readonly AgentRegistryClient _registry;
    private readonly ILogger<CibaCompletionService> _log;

    public CibaCompletionService(
        IBackchannelAuthenticationInteractionService interaction,
        IBackChannelAuthenticationRequestStore store,
        AgentRegistryClient registry,
        ILogger<CibaCompletionService> log)
    {
        _interaction = interaction;
        _store = store;
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
            await ResolvePendingForEdgeAsync(userId, parentType, childType, request.InternalId, subject);
        }
        return true;
    }

    /// <summary>
    /// Auto-completes any OTHER pending CIBA request for the same spawn edge once
    /// a consent has just been recorded. A consent-gated chain spawns all its
    /// links eagerly (spawning needs no token), so deeper links open their
    /// backchannel requests before the human approves the first one — and the
    /// notification hook only evaluates stored consent at request-creation time.
    /// Without this sweep those requests stay pending until their CibaLifetime
    /// lapses (~5 min), even though a covering consent now exists. Recording a
    /// consent is the event that should release them, so we do it here — the
    /// same decision the notification hook makes, applied to in-flight requests.
    /// Best-effort: a failure here never fails the human's own approval.
    /// </summary>
    private async Task ResolvePendingForEdgeAsync(
        string userId, string parentType, string childType,
        string justCompletedId, ClaimsPrincipal subject)
    {
        try
        {
            var logins = await _store.GetLoginsForUserAsync(userId);
            foreach (var stored in logins)
            {
                if (stored.IsComplete || stored.InternalId == justCompletedId) continue;
                if (CibaRequestValidator.Property(stored.Properties, CibaRequestValidator.ParentTypeKey) != parentType ||
                    CibaRequestValidator.Property(stored.Properties, CibaRequestValidator.ChildTypeKey) != childType)
                    continue;

                var pending = await _interaction.GetLoginRequestByInternalIdAsync(stored.InternalId);
                if (pending is null) continue;

                // Mirror the notification hook: only release scopes the consent covers.
                var decision = await _registry.CheckConsent(
                    userId, parentType, childType, pending.ValidatedResources.RawScopeValues);
                if (decision?.Granted != true) continue;

                await ApproveAsync(pending, subject, recordConsent: false);
                _log.LogInformation(
                    "CIBA released a pending request from the just-recorded consent: {UserId} {ParentType}->{ChildType}",
                    userId, parentType, childType);
            }
        }
        catch (Exception e)
        {
            _log.LogWarning("sweep of pending CIBA requests after consent record failed: {Error}", e.Message);
        }
    }

    public Task DenyAsync(BackchannelUserLoginRequest request, ClaimsPrincipal subject) =>
        _interaction.CompleteLoginRequestAsync(
            new CompleteBackchannelLoginRequest(request.InternalId) { Subject = subject });
}
