using Duende.IdentityServer.Services;

namespace IdentityServer;

/// <summary>
/// Bridges the registry-owned consent decision back to Duende's CIBA flow.
///
/// The registry (not the IdP) owns the pending->approved/denied lifecycle, so
/// when a user approves/denies in the dashboard the decision lands in the
/// registry — but the waiting agent is still polling Duende's token endpoint,
/// which only succeeds once the backchannel request is *completed*. Nothing in
/// the approve-at-the-registry path completes it.
///
/// This poller closes that gap while respecting the dependency direction (the
/// IdP polls the registry; the registry never calls the IdP). For each tracked
/// pending CIBA request it polls the registry's consent-request status and, when
/// it is no longer pending, completes (approve) or fails (deny) the Duende
/// request — the same effect the old in-IdP approval path had. Auto-approval
/// (a covering consent already exists) still completes inline in
/// CibaConsentNotificationService and never reaches the poller.
/// </summary>
public class ConsentCompletionPoller : BackgroundService
{
    private static readonly TimeSpan Interval = TimeSpan.FromSeconds(2);

    private readonly IServiceScopeFactory _scopes;
    private readonly AgentRegistryClient _registry;
    private readonly ConsentRequestTracker _tracker;
    private readonly ILogger<ConsentCompletionPoller> _log;

    public ConsentCompletionPoller(
        IServiceScopeFactory scopes,
        AgentRegistryClient registry,
        ConsentRequestTracker tracker,
        ILogger<ConsentCompletionPoller> log)
    {
        _scopes = scopes;
        _registry = registry;
        _tracker = tracker;
        _log = log;
    }

    protected override async Task ExecuteAsync(CancellationToken stoppingToken)
    {
        while (!stoppingToken.IsCancellationRequested)
        {
            try
            {
                await PollOnceAsync();
            }
            catch (Exception e)
            {
                _log.LogWarning("consent completion poll failed: {Error}", e.Message);
            }

            try { await Task.Delay(Interval, stoppingToken); }
            catch (OperationCanceledException) { break; }
        }
    }

    private async Task PollOnceAsync()
    {
        var tracked = _tracker.Snapshot();
        if (tracked.Count == 0) return;

        // Resolve the transient interaction/completion services within a scope
        // so they are not captured by this singleton hosted service.
        using var scope = _scopes.CreateScope();
        var interaction = scope.ServiceProvider.GetRequiredService<IBackchannelAuthenticationInteractionService>();
        var completion = scope.ServiceProvider.GetRequiredService<CibaCompletionService>();

        foreach (var (internalId, registryId) in tracked)
        {
            var cr = await _registry.GetConsentRequest(registryId);
            if (cr is null) continue;          // registry unreachable — retry next tick
            if (cr.Status == "pending") continue;

            var request = await interaction.GetLoginRequestByInternalIdAsync(internalId);
            if (request is null)
            {
                _tracker.Untrack(internalId);  // expired or already completed
                continue;
            }

            var userId = CibaRequestValidator.Property(request.Properties, CibaRequestValidator.UserIdKey) ?? "";
            var subject = CibaCompletionService.SubjectFor(userId);

            if (cr.Status == "approved")
            {
                // recordConsent:false — the registry already holds the grant.
                await completion.ApproveAsync(request, subject, recordConsent: false);
                _log.LogInformation("CIBA completed from registry approval: {InternalId}", internalId);
            }
            else if (cr.Status == "denied")
            {
                await completion.DenyAsync(request, subject);
                _log.LogInformation("CIBA failed from registry denial: {InternalId}", internalId);
            }

            _tracker.Untrack(internalId);
        }
    }
}
