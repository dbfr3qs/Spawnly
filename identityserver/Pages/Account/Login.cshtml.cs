using Duende.IdentityServer;
using Duende.IdentityServer.Test;
using Microsoft.AspNetCore.Authentication;
using Microsoft.AspNetCore.Mvc;
using Microsoft.AspNetCore.Mvc.RazorPages;

namespace IdentityServer.Pages.Account;

public class LoginModel : PageModel
{
    private readonly TestUserStore _users;

    public LoginModel(TestUserStore users) => _users = users;

    [BindProperty]
    public string Username { get; set; } = "";

    [BindProperty]
    public string Password { get; set; } = "";

    // The authorize endpoint redirects here with a local returnUrl
    // (/connect/authorize/callback?...); we round-trip it through the form.
    [BindProperty(SupportsGet = true)]
    public string? ReturnUrl { get; set; }

    public string? Error { get; set; }

    public void OnGet() { }

    public async Task<IActionResult> OnPostAsync()
    {
        if (_users.ValidateCredentials(Username, Password))
        {
            var user = _users.FindByUsername(Username);
            var principal = new IdentityServerUser(user.SubjectId)
            {
                DisplayName = user.Username,
            }.CreatePrincipal();

            await HttpContext.SignInAsync(principal);

            if (!string.IsNullOrEmpty(ReturnUrl) && Url.IsLocalUrl(ReturnUrl))
                return Redirect(ReturnUrl);
            return Redirect("~/");
        }

        Error = "Invalid username or password.";
        return Page();
    }
}
