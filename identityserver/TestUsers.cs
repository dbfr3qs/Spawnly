using System.Security.Claims;
using Duende.IdentityServer.Test;

namespace IdentityServer;

/// <summary>
/// Interactive (human) users for the example dashboard's OIDC login. This is the
/// machine-vs-human split: agents authenticate with SPIFFE JWT-SVIDs (see
/// <see cref="AgentClientSecretValidator"/>), while a person logs in here with a
/// username/password. Demo-grade only — a single seeded user, in memory.
/// </summary>
public static class TestUsers
{
    public static List<TestUser> Users =>
        new List<TestUser>
        {
            new TestUser
            {
                SubjectId = "alice",
                Username = "alice",
                Password = "alice",
                Claims =
                {
                    new Claim("name", "Alice"),
                    new Claim("preferred_username", "alice"),
                },
            },
        };
}
