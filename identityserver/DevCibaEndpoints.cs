using Duende.IdentityServer;
using Duende.IdentityServer.Services;
using Duende.IdentityServer.Stores;

namespace IdentityServer;

/// <summary>
/// Dev-only HTTP surface for inspecting and completing pending CIBA
/// (backchannel authentication) requests, so the flow can be driven with curl
/// before the dashboard consent UI exists. Mapped only when DEV_CIBA_API=true;
/// never enable in a real deployment — it lets the caller approve on behalf of
/// any user. The Phase-2 consent API replaces this with endpoints authenticated
/// by the user's own IdentityServer session.
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
            IBackchannelAuthenticationInteractionService interaction) =>
        {
            var request = await interaction.GetLoginRequestByInternalIdAsync(id);
            if (request is null) return Results.NotFound();

            var subject = new IdentityServerUser(user)
            {
                AuthenticationTime = DateTime.UtcNow,
                IdentityProvider = IdentityServerConstants.LocalIdentityProvider,
            }.CreatePrincipal();

            await interaction.CompleteLoginRequestAsync(new CompleteBackchannelLoginRequest(id)
            {
                Subject = subject,
                ScopesValuesConsented = request.ValidatedResources.RawScopeValues,
            });
            return Results.Ok(new { id, result = "approved" });
        });

        // Deny: complete with the validated user but no consented scopes — Duende
        // requires a Subject even on denial; the poll then returns access_denied.
        app.MapPost("/dev/ciba/requests/{id}/deny", async (
            string id, IBackchannelAuthenticationInteractionService interaction) =>
        {
            var request = await interaction.GetLoginRequestByInternalIdAsync(id);
            if (request is null) return Results.NotFound();

            await interaction.CompleteLoginRequestAsync(new CompleteBackchannelLoginRequest(id)
            {
                Subject = request.Subject,
            });
            return Results.Ok(new { id, result = "denied" });
        });
    }
}
