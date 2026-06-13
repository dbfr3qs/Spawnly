using Duende.IdentityServer.Models;
using Duende.IdentityServer.Services;

namespace IdentityServer;

/// <summary>
/// Duende calls this for every validated CIBA request — it is where "ask the
/// user only when needed" lives. The registry is asked to create (or return
/// the existing open) consent request for this (user, parentType, childType)
/// edge. If the registry short-circuits to "approved" (a stored consent
/// already covers the requested scopes), the CIBA request is completed
/// immediately and the first token poll succeeds without any human
/// interaction. Otherwise the request stays pending — the registry's own
/// notifier webhook (NOTIFIER_WEBHOOK_URL, read by the registry) and dashboard
/// take it from here.
/// </summary>
public class CibaConsentNotificationService : IBackchannelAuthenticationUserNotificationService
{
    private readonly CibaCompletionService _completion;
    private readonly AgentRegistryClient _registry;
    private readonly ILogger<CibaConsentNotificationService> _log;

    public CibaConsentNotificationService(
        CibaCompletionService completion,
        AgentRegistryClient registry,
        ILogger<CibaConsentNotificationService> log)
    {
        _completion = completion;
        _registry = registry;
        _log = log;
    }

    public async Task SendLoginRequestAsync(BackchannelUserLoginRequest request)
    {
        var userId = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.UserIdKey);
        var parentType = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.ParentTypeKey);
        var childType = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.ChildTypeKey);
        var scopes = request.ValidatedResources.RawScopeValues;

        if (userId is not { Length: > 0 } || parentType is not { Length: > 0 } || childType is not { Length: > 0 })
        {
            _log.LogWarning("CIBA request {InternalId} missing edge properties; cannot create consent request",
                request.InternalId);
            return;
        }

        var consentRequest = await _registry.CreateConsentRequest(
            userId, parentType, childType, scopes, request.BindingMessage, externalRef: request.InternalId);
        if (consentRequest is null)
        {
            _log.LogWarning(
                "CIBA pending user approval (registry unreachable): {UserId} {ParentType}->{ChildType}",
                userId, parentType, childType);
            return;
        }

        _completion.TrackConsentRequest(request.InternalId, consentRequest.Id);

        if (consentRequest.Status == "approved")
        {
            _log.LogInformation(
                "CIBA auto-approved from stored consent: {UserId} {ParentType}->{ChildType}",
                userId, parentType, childType);
            await _completion.ApproveAsync(request,
                CibaCompletionService.SubjectFor(userId), recordConsent: false);
            return;
        }

        _log.LogInformation(
            "CIBA pending user approval: {UserId} {ParentType}->{ChildType}",
            userId, parentType, childType);
    }
}
