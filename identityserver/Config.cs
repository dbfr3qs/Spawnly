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
            // The "role" user claim (value "admin" for an admin user) rides in
            // the id_token when a client requests the "roles" scope. The
            // dashboard/mobile read it to gate the admin UI; the orchestrator
            // reads the same claim from the access token (via the orchestrator
            // ApiResource's UserClaims below).
            new IdentityResource("roles", "User roles", new[] { "role" }),
        };

    public static IEnumerable<ApiScope> ApiScopes =>
        new List<ApiScope>
        {
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
            // Phase 0: an agent presents a token carrying this scope (audienced
            // for the orchestrator) to authenticate POST /spawn.
            new ApiScope("orchestrator:spawn", "Spawn child agents via the orchestrator"),
            // The dashboard (a confidential OIDC relying party acting for the
            // logged-in human) presents an orchestrator-audienced access token
            // carrying these to authenticate its calls: read for the read-only
            // routes (list/events/logs/templates/consents), write for every
            // mutating route (spawn/message/dismiss/delete/revoke/resume/...).
            new ApiScope("orchestrator:read", "Read via the orchestrator"),
            new ApiScope("orchestrator:write", "Act via the orchestrator"),
        };

    // ApiResources give the access token its audience (`aud`): the resource name is
    // emitted as an audience when one of its scopes is granted.
    public static IEnumerable<ApiResource> ApiResources =>
        new List<ApiResource>
        {
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
            // Gives a spawn token the audience the orchestrator validates
            // ("orchestrator") when an agent calls POST /spawn, and the same
            // audience to the dashboard's read/write access tokens.
            new ApiResource("orchestrator", "Agent Orchestrator")
            {
                Scopes = { "orchestrator:spawn", "orchestrator:read", "orchestrator:write" },
                // "role" rides in the orchestrator-audience access token so the
                // orchestrator can authorize admin-only operations (template
                // management) on it. The value comes from the user's "role"
                // claim (TestUsers); a non-admin user simply has no such claim.
                //
                // SECURITY INVARIANT: "role" must NEVER be a client-asserted
                // claim. It is sourced ONLY from the authenticated user's claims
                // (Duende DefaultClaimsService), never from a client's
                // AlwaysSendClientClaims assertion, and no IProfileService may
                // re-emit it on token-exchange or any other grant. Concretely:
                //   - the dashboard/mobile clients do NOT set
                //     AlwaysSendClientClaims, so a browser/app request can't
                //     inject a role;
                //   - the machine clients that DO set AlwaysSendClientClaims
                //     (weather-monitor, chain-worker, travel-*) cannot request
                //     the "roles" scope nor "orchestrator:write";
                //   - token-exchange (TokenExchangeGrantValidator) carries over
                //     only "sub", never "role".
                // If you add an IProfileService or grant a role claim to any
                // client, re-audit this: a client-asserted role would be
                // forgeable and would defeat the admin gate.
                UserClaims = { "role" },
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
                // offline_access mints a refresh token so the dashboard backend
                // can keep its short-lived orchestrator access token fresh for
                // the life of the 8h browser session without re-driving login.
                AllowOfflineAccess = true,
                // openid/profile drive the human's id_token; orchestrator:read /
                // orchestrator:write are the delegated authority the dashboard
                // presents to the orchestrator (aud=orchestrator, sub=userId).
                // NOT orchestrator:spawn — human spawn uses orchestrator:write;
                // spawn stays agent-only.
                AllowedScopes = { "openid", "profile", "roles", "orchestrator:read", "orchestrator:write" },
            },
            // Interactive human login for the native MOBILE app (iOS/Android).
            // Unlike the dashboard (a confidential backend relying party), a
            // native app cannot keep a secret, so this is a PUBLIC client:
            // authorization_code + PKCE with NO client secret. The app calls the
            // SAME orchestrator consent endpoints the dashboard does (via the
            // mobile-gateway), so it carries the same orchestrator:read/write
            // delegated scopes and aud=orchestrator — NOT orchestrator:spawn.
            new Client
            {
                ClientId = "mobile",
                ClientName = "Spawnly Mobile",
                AllowedGrantTypes = GrantTypes.Code,
                RequirePkce = true,
                // Public client: PKCE is the proof, there is no secret to check.
                RequireClientSecret = false,
                RequireConsent = false,
                // Native redirect target: the app's custom URI scheme. The dev
                // build (expo-dev-client) uses this same scheme, so no Expo Go
                // loopback redirect is needed.
                RedirectUris =
                {
                    "spawnly://auth",
                },
                PostLogoutRedirectUris = { "spawnly://auth" },
                // A refresh token lets the app silently renew its short-lived
                // orchestrator access token; it is stored in OS secure storage
                // (Keychain/Keystore). One-time-use rotation limits replay.
                AllowOfflineAccess = true,
                RefreshTokenUsage = TokenUsage.OneTimeOnly,
                AllowedScopes = { "openid", "profile", "offline_access", "roles", "orchestrator:read", "orchestrator:write" },
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
                // weather-monitor calls external weather APIs directly (no Spawnly
                // token), so it requests no resource scope.
                AllowedScopes = { "openid" },
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
                    // Phase 0: authenticate POST /spawn for the next chain link.
                    "orchestrator:spawn",
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
                // orchestrator:spawn: the planner spawns the three specialists.
                AllowedScopes = { "openid", "orchestrator:spawn" },
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
            // orchestrator:spawn so any agent type minted via this factory can
            // authenticate POST /spawn without a per-type IdP edit (a scope a
            // client can't request just 400s at the sidecar).
            AllowedScopes = { "openid", scope, "orchestrator:spawn" },
        };
}
