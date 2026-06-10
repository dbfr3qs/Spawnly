using Duende.IdentityServer.Services;

namespace IdentityServer;

/// <summary>
/// Browser-facing consent API for pending CIBA requests, authenticated by the
/// user's own IdentityServer session cookie (established by the dashboard's
/// OIDC login, which proxies this origin). That binding is the security model:
/// only the very user a request asks to authenticate can see or complete it —
/// Duende scopes the pending list to the session user, and completion is
/// signed with the session principal. The dashboard polls /ciba/pending and
/// renders approve/deny.
/// </summary>
public static class CibaConsentApi
{
    public static void MapCibaConsentApi(this WebApplication app)
    {
        app.MapGet("/ciba/pending", async (
            IBackchannelAuthenticationInteractionService interaction) =>
        {
            var pending = await interaction.GetPendingLoginRequestsForCurrentUserAsync();
            return Results.Json(pending.Select(r => new
            {
                id = r.InternalId,
                childType = CibaRequestValidator.Property(r.Properties, CibaRequestValidator.ChildTypeKey),
                parentType = CibaRequestValidator.Property(r.Properties, CibaRequestValidator.ParentTypeKey),
                agentId = CibaRequestValidator.Property(r.Properties, CibaRequestValidator.AgentIdKey),
                scopes = r.ValidatedResources.RawScopeValues,
                bindingMessage = r.BindingMessage,
                client = r.Client.ClientName ?? r.Client.ClientId,
            }));
        }).RequireAuthorization();

        app.MapPost("/ciba/pending/{id}/approve", (
            string id, HttpContext http,
            IBackchannelAuthenticationInteractionService interaction,
            IUserSession session, CibaCompletionService completion) =>
            CompleteAsync(id, approve: true, http, interaction, session, completion))
            .RequireAuthorization();

        app.MapPost("/ciba/pending/{id}/deny", (
            string id, HttpContext http,
            IBackchannelAuthenticationInteractionService interaction,
            IUserSession session, CibaCompletionService completion) =>
            CompleteAsync(id, approve: false, http, interaction, session, completion))
            .RequireAuthorization();
    }

    private static async Task<IResult> CompleteAsync(
        string id, bool approve, HttpContext http,
        IBackchannelAuthenticationInteractionService interaction,
        IUserSession session, CibaCompletionService completion)
    {
        var request = await interaction.GetLoginRequestByInternalIdAsync(id);
        if (request is null) return Results.NotFound();

        // The session user must be the user the request asks to authenticate.
        var sessionSub = http.User.FindFirst("sub")?.Value;
        if (sessionSub is null || request.Subject.FindFirst("sub")?.Value != sessionSub)
            return Results.Forbid();

        if (approve)
        {
            await completion.ApproveAsync(request, http.User,
                sessionId: await session.GetSessionIdAsync());
            return Results.Ok(new { id, result = "approved" });
        }
        await completion.DenyAsync(request, http.User);
        return Results.Ok(new { id, result = "denied" });
    }
}
