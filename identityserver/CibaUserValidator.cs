using System.Security.Claims;
using Duende.IdentityServer.Test;
using Duende.IdentityServer.Validation;

namespace IdentityServer;

/// <summary>
/// Resolves a CIBA request's <c>login_hint</c> to the human who must approve it.
/// The platform threads the spawning user's id into the child sidecar, which
/// sends it as the login_hint; tokens elsewhere carry the same identity as
/// "user:&lt;id&gt;", so both bare ("alice") and prefixed ("user:alice") forms are
/// accepted. Backed by the demo TestUserStore for now — the Phase-2 version also
/// cross-checks the hint against the requesting agent's registry record.
/// </summary>
public class CibaUserValidator : IBackchannelAuthenticationUserValidator
{
    private readonly TestUserStore _users;

    public CibaUserValidator(TestUserStore users) => _users = users;

    public Task<BackchannelAuthenticationUserValidationResult> ValidateRequestAsync(
        BackchannelAuthenticationUserValidatorContext context)
    {
        var hint = context.LoginHint;
        if (hint != null && hint.StartsWith("user:"))
            hint = hint["user:".Length..];

        var user = hint is null ? null : _users.FindByUsername(hint);
        if (user is null)
        {
            return Task.FromResult(new BackchannelAuthenticationUserValidationResult
            {
                Error = "unknown_user_id",
                ErrorDescription = $"no user matches login_hint",
            });
        }

        var identity = new ClaimsIdentity(
            new[] { new Claim("sub", user.SubjectId) }, "ciba");
        return Task.FromResult(new BackchannelAuthenticationUserValidationResult
        {
            Subject = new ClaimsPrincipal(identity),
        });
    }
}
