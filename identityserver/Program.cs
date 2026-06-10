using Duende.IdentityServer.Stores;
using Duende.IdentityServer.Validation;
using IdentityServer;
using Microsoft.AspNetCore.Http;

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

// Shared SPIRE JWT-SVID validator (client_assertion + actor_token).
builder.Services.AddSingleton(sp =>
    new SpireSvidValidator(sp.GetRequiredService<IHttpClientFactory>(), spireJwksUrl));

builder.Services.AddIdentityServer(options =>
    {
        options.IssuerUri = issuerUri;
        options.UserInteraction.LoginUrl = "/Account/Login";
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
builder.Services.AddTransient<CibaCompletionService>();
builder.Services.AddTransient<
    Duende.IdentityServer.Services.IBackchannelAuthenticationUserNotificationService,
    CibaConsentNotificationService>();

// Concrete default secret validator — SpireClientSecretValidator delegates to it
// for non-SPIFFE (normal client_secret) requests, e.g. the dashboard's code flow.
builder.Services.AddTransient<ClientSecretValidator>();

builder.Services.AddTransient<IClientSecretValidator>(sp =>
    new SpireClientSecretValidator(
        sp.GetRequiredService<IClientStore>(),
        sp.GetRequiredService<SpireSvidValidator>(),
        sp.GetRequiredService<ClientSecretValidator>()));

var app = builder.Build();
app.UseStaticFiles();
app.UseRouting();
app.UseIdentityServer();
app.UseAuthorization();
app.MapRazorPages();

// Pending-consent API for the dashboard, authenticated by the user's own
// IdentityServer session cookie.
app.MapCibaConsentApi();

// Dev-only CIBA inspection/completion API (curl-driven spike); see DevCibaEndpoints.
if (Environment.GetEnvironmentVariable("DEV_CIBA_API") == "true")
{
    app.MapDevCibaEndpoints();
}

app.Run();
