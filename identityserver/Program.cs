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

builder.Services.AddIdentityServer()
    .AddInMemoryApiScopes(Config.ApiScopes)
    .AddInMemoryClients(Config.Clients())
    .AddCustomTokenRequestValidator<AgentRegistryValidator>();

builder.Services.AddTransient<IClientSecretValidator>(sp =>
    new SpireClientSecretValidator(
        sp.GetRequiredService<IClientStore>(),
        sp.GetRequiredService<IHttpClientFactory>(),
        spireJwksUrl));

var app = builder.Build();
app.UseIdentityServer();
app.Run();
