using Duende.IdentityServer.Services;
using Microsoft.AspNetCore.Authentication;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.Mvc.RazorPages;

namespace IdentityServer.Pages.Account;

public class LogoutModel : PageModel
{
    private readonly IIdentityServerInteractionService _interaction;

    public LogoutModel(IIdentityServerInteractionService interaction) =>
        _interaction = interaction;

    public async Task<IActionResult> OnGet(string? logoutId)
    {
        // The dashboard sends the browser here (the end_session endpoint forwards
        // to LogoutUrl) with an id_token_hint + post_logout_redirect_uri, captured
        // in the logout context. We sign out the IdP cookie — killing the IdP
        // session — then bounce back to the dashboard.
        var ctx = await _interaction.GetLogoutContextAsync(logoutId);

        await HttpContext.SignOutAsync();

        if (!string.IsNullOrEmpty(ctx?.PostLogoutRedirectUri))
            return Redirect(ctx.PostLogoutRedirectUri);

        return Page();
    }
}
