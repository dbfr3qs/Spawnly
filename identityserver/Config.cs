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
            // travel-tools MCP server scopes — one per tool. The user consents to
            // a sub-agent receiving exactly one of these, which authorizes exactly
            // one MCP tool.
            new ApiScope("fx:read", "Convert currency (travel-tools)"),
            new ApiScope("flights:read", "Search flights (travel-tools)"),
            new ApiScope("hotels:read", "Search hotels (travel-tools)"),
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
            // The travel-tools MCP server validates that a token's aud contains
            // "travel-tools"; granting any travel-tools scope emits that audience.
            new ApiResource("travel-tools", "Travel Tools MCP server")
            {
                Scopes = { "fx:read", "flights:read", "hotels:read" },
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
            // The travel-planner orchestrator calls no protected resource itself (it
            // only spawns + A2A-calls the specialists), but register a client so any
            // sidecar token request resolves rather than 400ing, and so the parent
            // identity is well-formed.
            new Client
            {
                ClientId = "travel-planner",
                AllowedGrantTypes = new List<string> { GrantType.ClientCredentials },
                RequireClientSecret = true,
                ClientSecrets = { new Secret("placeholder".Sha256()) },
                AlwaysSendClientClaims = true,
                ClientClaimsPrefix = "",
                AllowedScopes = { "openid" },
            },
            // travel-tools specialists. Each is a least-privilege MCP-client agent
            // allowed EXACTLY ONE travel-tools scope (so its token can call only one
            // tool). client_credentials for direct (phase 3) spawns; CIBA for the
            // consent-gated spawn the travel-planner template requires (phase 4).
            // openid is required for the CIBA grant. The template now pairs each
            // scope with requireUserConsent so the consent->scope->tool guarantee
            // holds.
            TravelSpecialist("flight-search", "flights:read"),
            TravelSpecialist("hotel-search", "hotels:read"),
            TravelSpecialist("fx-converter", "fx:read"),
        };

    // One least-privilege MCP-client agent: client_credentials + CIBA, allowed
    // openid + exactly `scope`. Short 120s token lifetime — once these edges are
    // consent-gated (phase 4), a revoked/expired consent starves the agent within
    // that window.
    private static Client TravelSpecialist(string clientId, string scope) =>
        new Client
        {
            ClientId = clientId,
            AllowedGrantTypes = new List<string> { GrantType.ClientCredentials, CibaGrantType },
            RequireClientSecret = true,
            ClientSecrets = { new Secret("placeholder".Sha256()) },
            AlwaysSendClientClaims = true,
            ClientClaimsPrefix = "",
            AccessTokenLifetime = 120,
            CibaLifetime = 300,
            PollingInterval = 5,
            AllowedScopes = { "openid", scope },
        };
}
