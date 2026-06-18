using Duende.IdentityServer.Validation;
using Duende.IdentityServer.Stores;

namespace IdentityServer;

/// <summary>
/// Top-level client authentication at the token endpoint. Machine clients
/// (agents) authenticate with an attestation credential presented as a
/// <c>client_assertion</c>; this validator accepts those by verifying the
/// credential through the configured <see cref="IAgentCredentialVerifier"/>
/// (SPIFFE JWT-SVID by default, AWS IRSA token, ...).
///
/// Any request that is NOT an attestation client_assertion (e.g. the interactive
/// <c>dashboard</c> client doing authorization_code with a real client_secret)
/// is delegated to Duende's built-in <see cref="ClientSecretValidator"/>, which
/// performs normal secret validation. This lets human login coexist with the
/// machine-identity flows on a single token endpoint.
/// </summary>
public class AgentClientSecretValidator : IClientSecretValidator
{
    public const string JwtBearerAssertionType = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer";

    // AwsStsAssertionType marks a presigned STS GetCallerIdentity credential
    // (not a JWT). Mirrors Go attestor.AwsStsAssertionType.
    public const string AwsStsAssertionType = "urn:spawnly:params:aws-sts-getcalleridentity";

    /// <summary>
    /// True when the assertion type is one this platform's attestors present
    /// (a SPIFFE/OIDC JWT, or an AWS STS credential) — i.e. a machine-identity
    /// client_assertion rather than a client_secret. A deployment uses one
    /// attestor; accepting all known types here is harmless because the
    /// configured verifier rejects credentials it doesn't understand.
    /// </summary>
    public static bool IsAttestationAssertionType(string? assertionType) =>
        assertionType == JwtBearerAssertionType || assertionType == AwsStsAssertionType;

    /// <summary>
    /// The request's client_assertion, but only when its assertion type means
    /// THIS validator verified it against the attestor during client
    /// authentication. Downstream validators must use this — never read
    /// client_assertion straight from the raw form — or a caller could
    /// authenticate with a client_secret and smuggle an unvalidated assertion
    /// past them.
    /// </summary>
    public static string? ValidatedAssertion(System.Collections.Specialized.NameValueCollection? raw) =>
        IsAttestationAssertionType(raw?.Get("client_assertion_type"))
            ? raw!.Get("client_assertion")
            : null;

    private readonly IClientStore _clients;
    private readonly IAgentCredentialVerifier _verifier;
    private readonly ClientSecretValidator _inner;

    public AgentClientSecretValidator(
        IClientStore clients,
        IAgentCredentialVerifier verifier,
        ClientSecretValidator inner)
    {
        _clients = clients;
        _verifier = verifier;
        _inner = inner;
    }

    public async Task<ClientSecretValidationResult> ValidateAsync(HttpContext context)
    {
        var form = await context.Request.ReadFormAsync();
        var clientId = form["client_id"].FirstOrDefault();
        var assertion = form["client_assertion"].FirstOrDefault();
        var assertionType = form["client_assertion_type"].FirstOrDefault();

        // Not an attestation assertion — hand off to Duende's default secret
        // validation (client_secret_post / basic). This is the human-login
        // (dashboard) path.
        if (assertion is null || !IsAttestationAssertionType(assertionType))
            return await _inner.ValidateAsync(context);

        if (clientId is null) return Fail();

        var client = await _clients.FindClientByIdAsync(clientId);
        if (client is null) return Fail();

        var identity = await _verifier.Verify(assertion);
        if (identity is null) return Fail();

        return new ClientSecretValidationResult { IsError = false, Client = client };
    }

    private static ClientSecretValidationResult Fail() =>
        new ClientSecretValidationResult { IsError = true };
}
