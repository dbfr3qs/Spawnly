using Duende.IdentityServer.Models;
using Duende.IdentityServer.Services;

namespace IdentityServer;

/// <summary>
/// Duende calls this for every validated CIBA request — it is where "ask the
/// user only when needed" lives. If the registry holds a consent covering this
/// (user, parentType, childType) edge and the requested scopes, the request is
/// completed immediately and the first token poll succeeds without any human
/// interaction. Otherwise the request stays pending for the dashboard to pick
/// up, and the optional notifier webhook (NOTIFIER_WEBHOOK_URL) is pinged so
/// users not watching the dashboard learn a consent is waiting.
/// </summary>
public class CibaConsentNotificationService : IBackchannelAuthenticationUserNotificationService
{
    private readonly CibaCompletionService _completion;
    private readonly AgentRegistryClient _registry;
    private readonly IHttpClientFactory _httpFactory;
    private readonly ILogger<CibaConsentNotificationService> _log;

    public CibaConsentNotificationService(
        CibaCompletionService completion,
        AgentRegistryClient registry,
        IHttpClientFactory httpFactory,
        ILogger<CibaConsentNotificationService> log)
    {
        _completion = completion;
        _registry = registry;
        _httpFactory = httpFactory;
        _log = log;
    }

    public async Task SendLoginRequestAsync(BackchannelUserLoginRequest request)
    {
        var userId = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.UserIdKey);
        var parentType = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.ParentTypeKey);
        var childType = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.ChildTypeKey);
        var scopes = request.ValidatedResources.RawScopeValues;

        if (userId is { Length: > 0 } && parentType is { Length: > 0 } && childType is { Length: > 0 })
        {
            var decision = await _registry.CheckConsent(userId, parentType, childType, scopes);
            if (decision?.Granted == true)
            {
                _log.LogInformation(
                    "CIBA auto-approved from stored consent: {UserId} {ParentType}->{ChildType}",
                    userId, parentType, childType);
                await _completion.ApproveAsync(request,
                    CibaCompletionService.SubjectFor(userId), recordConsent: false);
                return;
            }
            _log.LogInformation(
                "CIBA pending user approval ({Reason}): {UserId} {ParentType}->{ChildType}",
                decision?.Reason ?? "registry unreachable", userId, parentType, childType);
        }

        await NotifyAsync(request, userId, parentType, childType, scopes);
    }

    // Best-effort webhook; the dashboard's polling is the canonical delivery.
    private async Task NotifyAsync(BackchannelUserLoginRequest request,
        string? userId, string? parentType, string? childType, IEnumerable<string> scopes)
    {
        var url = Environment.GetEnvironmentVariable("NOTIFIER_WEBHOOK_URL");
        if (string.IsNullOrEmpty(url)) return;
        try
        {
            await _httpFactory.CreateClient().PostAsJsonAsync(url, new
            {
                type = "consent_pending",
                user = userId,
                parentType,
                childType,
                scopes,
                bindingMessage = request.BindingMessage,
            });
        }
        catch (Exception e)
        {
            _log.LogWarning("consent notifier webhook failed: {Error}", e.Message);
        }
    }
}
