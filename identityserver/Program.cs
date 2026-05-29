using Duende.IdentityServer.Stores;
using Duende.IdentityServer.Validation;
using IdentityServer;

var spireJwksUrl = Environment.GetEnvironmentVariable("SPIRE_JWKS_URL")
    ?? "http://spire-spiffe-oidc-discovery-provider.spire-system/.well-known/jwks.json";
var registryUrl = Environment.GetEnvironmentVariable("REGISTRY_URL")
    ?? "http://registry:8080";

var builder = WebApplication.CreateBuilder(args);
builder.Services.AddHttpClient();
builder.Services.AddHttpClient("spire").ConfigurePrimaryHttpMessageHandler(() =>
    new HttpClientHandler { ServerCertificateCustomValidationCallback = (_, _, _, _) => true });
builder.Services.AddSingleton(new AgentRegistryClient(registryUrl));

// Shared SPIRE JWT-SVID validator (client_assertion + actor_token).
builder.Services.AddSingleton(sp =>
    new SpireSvidValidator(sp.GetRequiredService<IHttpClientFactory>(), spireJwksUrl));

builder.Services.AddIdentityServer()
    .AddInMemoryApiScopes(Config.ApiScopes)
    .AddInMemoryApiResources(Config.ApiResources)
    .AddInMemoryClients(Config.Clients())
    .AddCustomTokenRequestValidator<AgentRegistryValidator>()
    .AddExtensionGrantValidator<TokenExchangeGrantValidator>();

builder.Services.AddTransient<IClientSecretValidator>(sp =>
    new SpireClientSecretValidator(
        sp.GetRequiredService<IClientStore>(),
        sp.GetRequiredService<SpireSvidValidator>()));

var app = builder.Build();
app.UseIdentityServer();
app.Run();
