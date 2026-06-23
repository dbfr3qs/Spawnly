using System.Security.Claims;
using Duende.IdentityServer.Test;

namespace IdentityServer;

/// <summary>
/// Interactive (human) users for the example dashboard's OIDC login. This is the
/// machine-vs-human split: agents authenticate with SPIFFE JWT-SVIDs (see
/// <see cref="AgentClientSecretValidator"/>), while a person logs in here with a
/// username/password.
///
/// The credential is injected from the environment (DASHBOARD_USER /
/// DASHBOARD_PASSWORD), sourced from the optional <c>dashboard-user</c> Secret —
/// <c>alice</c>/<c>alice</c> for the local kind demo (scripts/bootstrap.sh), a
/// strong generated password for the public AWS deploy (deploy/aws/deploy.sh).
///
/// Fail closed: with no password configured there is NO interactive user, so the
/// dashboard cannot be logged into. That is deliberately safer than shipping a
/// guessable default on a publicly reachable site.
/// </summary>
public static class TestUsers
{
    public static List<TestUser> Users
    {
        get
        {
            var username = Environment.GetEnvironmentVariable("DASHBOARD_USER") ?? "admin";
            var password = Environment.GetEnvironmentVariable("DASHBOARD_PASSWORD");
            if (string.IsNullOrEmpty(password))
                return new List<TestUser>();

            return new List<TestUser>
            {
                new TestUser
                {
                    SubjectId = username,
                    Username = username,
                    Password = password,
                    Claims =
                    {
                        new Claim("name", username),
                        new Claim("preferred_username", username),
                    },
                },
            };
        }
    }
}
