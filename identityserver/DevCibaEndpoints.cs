using Duende.IdentityServer.Services;
using Duende.IdentityServer.Stores;

namespace IdentityServer;

/// <summary>
/// Dev-only HTTP surface for inspecting and completing pending CIBA
/// (backchannel authentication) requests, so the flow can be driven with curl
/// without a browser session. Mapped only when DEV_CIBA_API=true; never enable
/// in a real deployment — it lets the caller approve on behalf of any user.
/// The production approval path is the registry-native consent broker
/// (POST /v1/consent-requests/{id}/approve), surfaced in the dashboard.
/// Completion goes through <see cref="CibaCompletionService"/>, so a dev
/// approval also records the consent in the registry, exactly like a real one.
/// </summary>
public static class DevCibaEndpoints
{
    public static void MapDevCibaEndpoints(this WebApplication app)
    {
        // Pending backchannel requests for a user (login_hint subject).
        app.MapGet("/dev/ciba/requests", async (
            string user, IBackChannelAuthenticationRequestStore store) =>
        {
            var logins = await store.GetLoginsForUserAsync(user);
            return Results.Json(logins
                .Where(r => !r.IsComplete)
                .Select(r => new
                {
                    id = r.InternalId,
                    clientId = r.ClientId,
                    scopes = r.RequestedScopes,
                    bindingMessage = r.BindingMessage,
                    expiresAt = r.CreationTime.AddSeconds(r.Lifetime),
                }));
        });

        // Approve: complete the request as the given user, consenting to all
        // requested scopes. The next token-endpoint poll returns tokens.
        app.MapPost("/dev/ciba/requests/{id}/approve", async (
            string id, string user,
            IBackchannelAuthenticationInteractionService interaction,
            CibaCompletionService completion) =>
        {
            var request = await interaction.GetLoginRequestByInternalIdAsync(id);
            if (request is null) return Results.NotFound();

            await completion.ApproveAsync(request, CibaCompletionService.SubjectFor(user));
            return Results.Ok(new { id, result = "approved" });
        });

        // Deny: complete with the validated user but no consented scopes — Duende
        // requires a Subject even on denial; the poll then returns access_denied.
        app.MapPost("/dev/ciba/requests/{id}/deny", async (
            string id, IBackchannelAuthenticationInteractionService interaction,
            CibaCompletionService completion) =>
        {
            var request = await interaction.GetLoginRequestByInternalIdAsync(id);
            if (request is null) return Results.NotFound();

            await completion.DenyAsync(request, request.Subject);
            return Results.Ok(new { id, result = "denied" });
        });
    }
}
