using Duende.IdentityServer.Models;

namespace IdentityServer;

public static class Config
{
    public const string TokenExchangeGrantType = "urn:ietf:params:oauth:grant-type:token-exchange";
    public const string CibaGrantType = "urn:openid:params:grant-type:ciba";

    // The dashboard's public origin (browser-facing). Used to build the OIDC
    // client's redirect/post-logout URIs. Kept in sync with the dashboard's
    // OIDC_AUTHORITY and IdentityServer's IssuerUri so there is a single origin.
    public static string DashboardOrigin =>
        Environment.GetEnvironmentVariable("DASHBOARD_ORIGIN") ?? "http://localhost:8090";

    // Interactive (human) login resources: openid for the id_token's sub,
    // profile for name/preferred_username.
    public static IEnumerable<IdentityResource> IdentityResources =>
        new List<IdentityResource>
        {
            new IdentityResources.OpenId(),
            new IdentityResources.Profile(),
        };

    public static IEnumerable<ApiScope> ApiScopes =>
        new List<ApiScope>
        {
            new ApiScope("sample-api", "Sample API"), // backward compat
            new ApiScope("sample-api-a:read", "Read sample-api-a"),
            new ApiScope("sample-api-a:write", "Write sample-api-a"),
            new ApiScope("sample-api-b:read", "Read sample-api-b"),
            new ApiScope("sample-api-b:write", "Write sample-api-b"),
            // Control-plane scope: the orchestrator and the IdP's own CIBA driver
            // present a client-credentials token carrying this scope to call the
            // registry's consent endpoints when CONTROL_PLANE_AUTH=oidc.
            new ApiScope("registry.consent", "Registry consent broker"),
        };

    // ApiResources give the access token its audience (`aud`): the resource name is
    // emitted as an audience when one of its scopes is granted.
    public static IEnumerable<ApiResource> ApiResources =>
        new List<ApiResource>
        {
            // Backs the "sample-api" scope so tokens requesting it carry
            // aud="sample-api" (the audience the sample-api resource server
            // enforces). Without this, scope-only tokens have an empty aud.
            new ApiResource("sample-api", "Sample API")
            {
                Scopes = { "sample-api" },
            },
            new ApiResource("sample-api-a", "Sample API A")
            {
                Scopes = { "sample-api-a:read", "sample-api-a:write" },
            },
            new ApiResource("sample-api-b", "Sample API B")
            {
                Scopes = { "sample-api-b:read", "sample-api-b:write" },
            },
            // Gives control-plane tokens the audience the registry validates
            // (CONTROL_PLANE_OIDC_AUDIENCE, default "registry").
            new ApiResource("registry", "Agent Registry")
            {
                Scopes = { "registry.consent" },
            },
        };

    public static IEnumerable<Client> Clients() =>
        new List<Client>
        {
            // Interactive human login for the example dashboard (the only client
            // that uses authorization_code + a real client_secret; all others are
            // machine clients authenticated via SPIFFE JWT-SVID). The dashboard's
            // Go backend is a confidential relying party.
            new Client
            {
                ClientId = "dashboard",
                ClientName = "Spawnly Dashboard",
                AllowedGrantTypes = GrantTypes.Code,
                RequirePkce = true,
                RequireConsent = false,
                // Real secret (env-overridable for the deployed manifest). The
                // AgentClientSecretValidator delegates non-SPIFFE requests to
                // Duende's default validator, which checks this hash.
                ClientSecrets =
                {
                    new Secret(
                        (Environment.GetEnvironmentVariable("DASHBOARD_CLIENT_SECRET") ?? "dashboard-secret")
                            .Sha256()),
                },
                RedirectUris = { $"{DashboardOrigin}/callback" },
                PostLogoutRedirectUris = { $"{DashboardOrigin}/" },
                AllowedScopes = { "openid", "profile" },
            },
            // Control-plane clients (CONTROL_PLANE_AUTH=oidc). Unlike the agent
            // clients above — authenticated via SPIFFE JWT-SVID with a
            // "placeholder" secret — these are platform services authenticating
            // with a real client_secret (validated by Duende's default
            // validator), so they can mint a client-credentials token for the
            // registry's consent endpoints. Inert unless the registry is run
            // with CONTROL_PLANE_AUTH=oidc; the local demo leaves it "none".
            new Client
            {
                ClientId = "orchestrator",
                ClientName = "Spawnly Orchestrator",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                ClientSecrets =
                {
                    new Secret(
                        (Environment.GetEnvironmentVariable("ORCHESTRATOR_CLIENT_SECRET") ?? "orchestrator-secret")
                            .Sha256()),
                },
                AllowedScopes = { "registry.consent" },
            },
            new Client
            {
                // The IdP's CIBA driver (AgentRegistryClient) calling the
                // registry's consent endpoints — IdentityServer acting as a
                // client of itself.
                ClientId = "idp-consent",
                ClientName = "Spawnly IdP Consent Driver",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                ClientSecrets =
                {
                    new Secret(
                        (Environment.GetEnvironmentVariable("IDP_CONSENT_CLIENT_SECRET") ?? "idp-consent-secret")
                            .Sha256()),
                },
                AllowedScopes = { "registry.consent" },
            },
            new Client
            {
                ClientId = "worker",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                // Placeholder so Duende's config validator is satisfied;
                // actual auth is via AgentClientSecretValidator (client_assertion JWT-SVID).
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api" },
            },
            new Client
            {
                ClientId = "weather-monitor",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api" },
            },
            new Client
            {
                ClientId = "global-worker",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api-a:read", "sample-api-a:write" },
            },
            new Client
            {
                ClientId = "pi-worker",
                AllowedGrantTypes = GrantTypes.ClientCredentials,
                RequireClientSecret = true,
                // Placeholder so Duende's config validator is satisfied;
                // actual auth is via AgentClientSecretValidator (client_assertion JWT-SVID).
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "sample-api-a:read" },
            },
            new Client
            {
                ClientId = "parent-agent",
                // Both client_credentials (to mint a root token) and token-exchange
                // (to delegate to a child / re-exchange).
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    TokenExchangeGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                // Short-lived delegated tokens (Milestone 3): a revocation backstop so an
                // in-flight token cannot be used long after its chain is revoked.
                AccessTokenLifetime = 120,
                AllowedScopes =
                {
                    "sample-api-a:read",
                    "sample-api-a:write",
                    "sample-api-b:read",
                },
            },
            new Client
            {
                ClientId = "child-agent",
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    TokenExchangeGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                // Short-lived delegated tokens (Milestone 3): a revocation backstop so an
                // in-flight token cannot be used long after its chain is revoked.
                AccessTokenLifetime = 120,
                AllowedScopes =
                {
                    "sample-api-b:read",
                },
            },
            new Client
            {
                ClientId = "trip-planner",
                // Both client_credentials (to mint a root token) and token-exchange
                // (to delegate to a child / re-exchange).
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    TokenExchangeGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                // Short-lived delegated tokens (Milestone 3): a revocation backstop so an
                // in-flight token cannot be used long after its chain is revoked.
                AccessTokenLifetime = 120,
                AllowedScopes =
                {
                    "sample-api-a:read",
                    "sample-api-a:write",
                    "sample-api-b:read",
                },
            },
            new Client
            {
                ClientId = "currency-converter",
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    TokenExchangeGrantType,
                    // CIBA (spike): the child agent runs a backchannel authentication
                    // request at spawn so the human approves the handoff before any
                    // user-bound token is minted.
                    CibaGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                // Short-lived delegated tokens (Milestone 3): a revocation backstop so an
                // in-flight token cannot be used long after its chain is revoked.
                AccessTokenLifetime = 120,
                // How long a pending backchannel request may wait for the human
                // (expired_token on poll afterwards), and the minimum poll interval.
                CibaLifetime = 300,
                PollingInterval = 5,
                AllowedScopes =
                {
                    // CIBA is an OIDC authentication request, so openid is required.
                    "openid",
                    "sample-api-b:read",
                },
            },
            new Client
            {
                // Long-lived self-spawning worker used to demonstrate cascading
                // revocation across an agent chain. Each link calls sample-api-a
                // with its OWN client-credentials token, so revoking a node (and
                // its subtree) denies each one independently on its next call.
                ClientId = "chain-worker",
                AllowedGrantTypes = new List<string>
                {
                    GrantType.ClientCredentials,
                    // CIBA: every spawned link is consent-gated by the parent
                    // template, so its sidecar runs the backchannel flow.
                    CibaGrantType,
                },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                // Short token lifetime so a revoked link cannot keep using an
                // in-flight token long after its authorization is dropped (the
                // resource also denies in real time via the SpiceDB check).
                AccessTokenLifetime = 120,
                CibaLifetime = 300,
                PollingInterval = 5,
                AllowedScopes =
                {
                    // openid: CIBA is an OIDC authentication request.
                    "openid",
                    "sample-api-a:read",
                },
            },
        };
}
