using Duende.IdentityServer.Stores;
using Duende.IdentityServer.Validation;
using IdentityServer;
using Microsoft.AspNetCore.Http;
using Microsoft.AspNetCore.HttpOverrides;

var spireJwksUrl = Environment.GetEnvironmentVariable("SPIRE_JWKS_URL")
    ?? "http://spire-spiffe-oidc-discovery-provider.spire-system/.well-known/jwks.json";
var registryUrl = Environment.GetEnvironmentVariable("REGISTRY_URL")
    ?? "http://registry:8080";
// Token issuer. This MUST stay the in-cluster identity-server URL because the
// resource servers (sample-api) validate every token's `iss` against it — for
// agent tokens and human id_tokens alike. The browser never needs this value;
// it reaches the authorize/login endpoints through the dashboard's proxy.
var issuerUri = Environment.GetEnvironmentVariable("ISSUER_URI")
    ?? "http://identity-server:8080";

var builder = WebApplication.CreateBuilder(args);
builder.Services.AddHttpClient();
builder.Services.AddHttpClient("spire").ConfigurePrimaryHttpMessageHandler(() =>
    new HttpClientHandler { ServerCertificateCustomValidationCallback = (_, _, _, _) => true });
builder.Services.AddSingleton(new AgentRegistryClient(registryUrl));

// Razor pages host the interactive login UI (machine clients never touch these).
builder.Services.AddRazorPages();

// SPIRE JWT-SVID validator (used by the SPIFFE credential verifier).
builder.Services.AddSingleton(sp =>
    new SpireSvidValidator(sp.GetRequiredService<IHttpClientFactory>(), spireJwksUrl));

// Pluggable attestation: the verifier authenticates an agent's credential
// (client_assertion / actor_token) and derives its identity. Default is
// SPIFFE/SPIRE; other attestors (AWS IRSA, ...) select in here via ATTESTOR.
// Its AgentId derivation MUST match the registry's registrant.Verifier.
var attestor = Environment.GetEnvironmentVariable("ATTESTOR") ?? "spiffe";

// aws-stsweb config (STS outbound web identity federation + EKS Pod Identity).
var stswebIssuer = Environment.GetEnvironmentVariable("STSWEB_ISSUER") ?? "";
var stswebJwks = Environment.GetEnvironmentVariable("STSWEB_JWKS_URL") ?? "";
if (string.IsNullOrEmpty(stswebJwks) && !string.IsNullOrEmpty(stswebIssuer))
    stswebJwks = stswebIssuer.TrimEnd('/') + "/.well-known/jwks.json";
var stswebOptions = new StsWebOptions(
    stswebJwks, stswebIssuer,
    Environment.GetEnvironmentVariable("STSWEB_AUDIENCE") ?? "spawnly",
    Environment.GetEnvironmentVariable("STSWEB_NAMESPACE") ?? "",
    Environment.GetEnvironmentVariable("STSWEB_SERVICE_ACCOUNT") ?? "",
    Environment.GetEnvironmentVariable("STSWEB_CLUSTER_ARN") ?? "",
    "");

builder.Services.AddSingleton<IAgentCredentialVerifier>(sp => attestor switch
{
    "" or "spiffe" => new SpireCredentialVerifier(sp.GetRequiredService<SpireSvidValidator>()),
    "aws-sts" => new StsCredentialVerifier(sp.GetRequiredService<IHttpClientFactory>()),
    "aws-stsweb" => new StsWebCredentialVerifier(
        sp.GetRequiredService<IHttpClientFactory>(), stswebOptions,
        sp.GetRequiredService<ILogger<StsWebCredentialVerifier>>()),
    _ => throw new InvalidOperationException($"unknown ATTESTOR '{attestor}'"),
});

builder.Services.AddIdentityServer(options =>
    {
        options.IssuerUri = issuerUri;
        // Served at /login (Login.cshtml @page route), proxied by the dashboard
        // so the browser sees a clean spawnly.run/login URL.
        options.UserInteraction.LoginUrl = "/login";
        options.UserInteraction.LogoutUrl = "/Account/Logout";
        // This demo runs over plain HTTP. Duende defaults the session cookie to
        // SameSite=None, which browsers drop unless it is also Secure (and Secure
        // cookies aren't sent over HTTP). Everything here is one origin, so Lax
        // both satisfies the browser and is sufficient for the flow.
        options.Authentication.CookieSameSiteMode = SameSiteMode.Lax;
        options.Authentication.CheckSessionCookieSameSiteMode = SameSiteMode.Lax;
    })
    .AddInMemoryIdentityResources(Config.IdentityResources)
    .AddInMemoryApiScopes(Config.ApiScopes)
    .AddInMemoryApiResources(Config.ApiResources)
    .AddInMemoryClients(Config.Clients())
    .AddTestUsers(TestUsers.Users)
    .AddCustomTokenRequestValidator<AgentRegistryValidator>()
    .AddExtensionGrantValidator<TokenExchangeGrantValidator>()
    // CIBA: resolves a backchannel request's login_hint to the approving user,
    // then binds the request to a registry-derived spawn edge.
    .AddBackchannelAuthenticationUserValidator<CibaUserValidator>()
    .AddCustomBackchannelAuthenticationRequestValidator<CibaRequestValidator>();

// CIBA consent plumbing: completion (approve/deny + consent recording) and the
// notification hook that auto-approves from stored consent or leaves the
// request pending for the dashboard (optionally pinging NOTIFIER_WEBHOOK_URL).
builder.Services.AddSingleton<ConsentRequestTracker>();
builder.Services.AddTransient<CibaCompletionService>();
builder.Services.AddTransient<
    Duende.IdentityServer.Services.IBackchannelAuthenticationUserNotificationService,
    CibaConsentNotificationService>();
// Bridges registry-owned consent decisions back to Duende: polls the registry
// for each tracked pending CIBA request and completes/fails it on resolution.
builder.Services.AddHostedService<ConsentCompletionPoller>();

// Concrete default secret validator — AgentClientSecretValidator delegates to it
// for non-attestation (normal client_secret) requests, e.g. the dashboard's code flow.
builder.Services.AddTransient<ClientSecretValidator>();

builder.Services.AddTransient<IClientSecretValidator>(sp =>
    new AgentClientSecretValidator(
        sp.GetRequiredService<IClientStore>(),
        sp.GetRequiredService<IAgentCredentialVerifier>(),
        sp.GetRequiredService<ClientSecretValidator>()));

var app = builder.Build();

// Behind a TLS-terminating proxy (the EKS ALB → the dashboard's OIDC reverse
// proxy), honor X-Forwarded-Proto so the request scheme is seen as https. That
// makes IdentityServer's session/antiforgery cookies Secure over the public
// origin. Off by default so the local HTTP flow (kind / port-forward) is
// unaffected; deploy.sh sets FORWARDED_HEADERS=true on the public AWS deploy.
// KnownNetworks/KnownProxies are cleared because identity-server is a ClusterIP
// reachable only via in-cluster hops, so the forwarded header is trusted.
if (Environment.GetEnvironmentVariable("FORWARDED_HEADERS") == "true")
{
    var forwardedOptions = new ForwardedHeadersOptions
    {
        ForwardedHeaders = ForwardedHeaders.XForwardedProto,
    };
    forwardedOptions.KnownNetworks.Clear();
    forwardedOptions.KnownProxies.Clear();
    app.UseForwardedHeaders(forwardedOptions);
}

app.UseStaticFiles();
app.UseRouting();
app.UseIdentityServer();
app.UseAuthorization();
app.MapRazorPages();

// Dev-only CIBA inspection/completion API (curl-driven spike); see DevCibaEndpoints.
if (Environment.GetEnvironmentVariable("DEV_CIBA_API") == "true")
{
    app.MapDevCibaEndpoints();
}

app.Run();
