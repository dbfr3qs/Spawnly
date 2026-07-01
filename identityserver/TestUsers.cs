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
/// strong generated password for the public AWS deploy.
///
/// Fail closed: with no password configured there is NO interactive user, so the
/// dashboard cannot be logged into. That is deliberately safer than shipping a
/// guessable default on a publicly reachable site.
///
/// Roles: the primary user carries the <c>role=admin</c> claim, which the
/// dashboard/orchestrator use to gate agent-type (template) management. An
/// optional SECOND, non-admin user (<c>DASHBOARD_VIEWER_USER</c> /
/// <c>DASHBOARD_VIEWER_PASSWORD</c>, defaulting to <c>viewer</c>/<c>viewer</c>
/// for the local demo) carries NO role claim, so authz-deny paths and e2e can be
/// exercised. It is fail-closed in exactly the same way as the primary user: no
/// <c>DASHBOARD_VIEWER_PASSWORD</c> ⇒ no viewer user, so a public deploy never
/// ships a guessable non-admin login either.
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

            var users = new List<TestUser>
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
                        // Admin role: this user can manage agent types (templates)
                        // from the dashboard. Ordinary users are defined without
                        // this claim.
                        new Claim("role", "admin"),
                    },
                },
            };

            // Optional non-admin user for exercising authz-deny paths (the
            // dashboard's Agent Types admin view, the orchestrator's admin
            // routes) and the e2e suite. Fail-closed like the primary user: it
            // exists only when DASHBOARD_VIEWER_PASSWORD is set, so a public
            // deploy that doesn't set it ships no guessable login. The local
            // kind demo (scripts/bootstrap.sh) sets viewer/viewer.
            var viewerUser = Environment.GetEnvironmentVariable("DASHBOARD_VIEWER_USER") ?? "viewer";
            var viewerPassword = Environment.GetEnvironmentVariable("DASHBOARD_VIEWER_PASSWORD");
            if (!string.IsNullOrEmpty(viewerPassword))
            {
                users.Add(new TestUser
                {
                    SubjectId = viewerUser,
                    Username = viewerUser,
                    Password = viewerPassword,
                    Claims =
                    {
                        new Claim("name", viewerUser),
                        new Claim("preferred_username", viewerUser),
                        // Intentionally NO "role" claim: an ordinary user.
                    },
                });
            }

            return users;
        }
    }
}
