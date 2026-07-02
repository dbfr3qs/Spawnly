package run.spawnly.keycloak.authenticator;

import jakarta.ws.rs.core.MultivaluedMap;
import jakarta.ws.rs.core.Response;
import org.jboss.logging.Logger;
import org.keycloak.OAuth2Constants;
import org.keycloak.authentication.AuthenticationFlowError;
import org.keycloak.authentication.ClientAuthenticationFlowContext;
import org.keycloak.authentication.authenticators.client.AbstractClientAuthenticator;
import org.keycloak.authentication.authenticators.client.ClientAuthUtil;
import org.keycloak.models.AuthenticationExecutionModel;
import org.keycloak.models.ClientModel;
import org.keycloak.protocol.oidc.OIDCLoginProtocol;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.sessions.AuthenticationSessionModel;

import run.spawnly.keycloak.attestor.AgentCredentialVerifier;
import run.spawnly.keycloak.attestor.AgentIdentity;
import run.spawnly.keycloak.attestor.AttestorFactory;

import java.util.Collections;
import java.util.List;
import java.util.Set;

/**
 * Top-level client authentication at the token endpoint for machine (agent)
 * clients. Ported from identityserver/AgentClientSecretValidator.cs.
 *
 * <p>Agents authenticate with an attestation credential presented as a
 * {@code client_assertion}; this authenticator accepts the two Spawnly
 * assertion types and verifies the credential through the configured
 * {@link AgentCredentialVerifier} (SPIFFE JWT-SVID by default, AWS STS in
 * Phase 5). On success it resolves the client by {@code client_id} and stores
 * the verified {@code {subject, agentId}} in the auth session's CLIENT NOTES,
 * which propagate to the authenticated client session and are read by the
 * Phase 2 protocol mappers (sub override + act chain).
 *
 * <p>NON-ATTESTATION requests are not this authenticator's business: a request
 * without a Spawnly {@code client_assertion_type} is {@code attempted()} so the
 * flow's built-in client-secret authenticator (client_secret_post / basic)
 * handles human + control-plane clients — exactly the delegate-to-default
 * behaviour of the C# {@code AgentClientSecretValidator}. Configuring this as an
 * ALTERNATIVE next to the secret authenticator in the client-auth flow keeps
 * both paths working on one token endpoint.
 *
 * <p>SECURITY: the verified identity is stored under note keys the mappers key
 * on; a client_secret client can never reach this code path (its request has no
 * Spawnly assertion type), so it can never populate those notes — the mapper
 * trust boundary is "only an attestation-authenticated session carries them".
 */
public class SpawnlyAttestationAuthenticator extends AbstractClientAuthenticator {

    public static final String PROVIDER_ID = "spawnly-attestation";
    private static final Logger log = Logger.getLogger(SpawnlyAttestationAuthenticator.class);

    // Assertion types this platform's attestors present. Mirrors the C#
    // AgentClientSecretValidator constants and Go attestor.*AssertionType.
    public static final String JWT_BEARER_ASSERTION_TYPE =
            "urn:ietf:params:oauth:client-assertion-type:jwt-bearer";
    public static final String AWS_STS_ASSERTION_TYPE =
            "urn:spawnly:params:aws-sts-getcalleridentity";

    // Client-session note keys carrying the verified identity to the Phase 2
    // mappers. Namespaced so they can't collide with Keycloak's own notes.
    public static final String NOTE_AGENT_SUBJECT = "spawnly.agent.subject";
    public static final String NOTE_AGENT_ID = "spawnly.agent.id";
    public static final String NOTE_AGENT_ISSUER = "spawnly.agent.issuer";

    // The verifier is selected once from ATTESTOR at factory init and shared
    // (stateless, thread-safe). AbstractClientAuthenticator uses ONE instance as
    // both the factory and the authenticator (create() returns this), so the
    // verifier is a mutable field populated in init(). An unknown ATTESTOR makes
    // init() throw — a HARD FAIL at server startup, matching Program.cs.
    private volatile AgentCredentialVerifier verifier;

    /** Keycloak instantiates providers via a public no-arg constructor. */
    public SpawnlyAttestationAuthenticator() {
    }

    /** Test seam: inject a verifier directly, bypassing ATTESTOR selection. */
    SpawnlyAttestationAuthenticator(AgentCredentialVerifier verifier) {
        this.verifier = verifier;
    }

    @Override
    public void init(org.keycloak.Config.Scope config) {
        // Select the attestor once. Unknown value ⇒ IllegalStateException ⇒
        // startup fails loudly (no silent insecure fallback).
        this.verifier = AttestorFactory.build(config);
    }

    /** True for a client_assertion type one of this platform's attestors uses. */
    static boolean isAttestationAssertionType(String type) {
        return JWT_BEARER_ASSERTION_TYPE.equals(type) || AWS_STS_ASSERTION_TYPE.equals(type);
    }

    @Override
    public void authenticateClient(ClientAuthenticationFlowContext context) {
        MultivaluedMap<String, String> params = context.getHttpRequest().getDecodedFormParameters();
        String assertionType = params.getFirst(OAuth2Constants.CLIENT_ASSERTION_TYPE);
        String assertion = params.getFirst(OAuth2Constants.CLIENT_ASSERTION);

        // Not a Spawnly attestation assertion → not our business. attempted()
        // lets the built-in client-secret authenticator (the human/control-plane
        // path) handle it. This is the delegate-to-default in the C# validator.
        if (assertion == null || !isAttestationAssertionType(assertionType)) {
            context.attempted();
            return;
        }

        String clientId = params.getFirst(OAuth2Constants.CLIENT_ID);
        if (clientId == null || clientId.isEmpty()) {
            fail(context, "missing client_id");
            return;
        }

        ClientModel client = context.getRealm().getClientByClientId(clientId);
        if (client == null) {
            fail(context, "unknown client");
            return;
        }
        // Bind the client early so downstream sees who's authenticating even on
        // a subsequent failure (matches Keycloak's built-in authenticators).
        context.getEvent().client(clientId);
        context.setClient(client);

        AgentIdentity identity;
        try {
            identity = verifier.verify(assertion);
        } catch (RuntimeException e) {
            // Defensive: the seam contract says verify() returns null on a bad
            // credential, but never let an unexpected error surface as a 500.
            log.debugf(e, "attestation verify threw for client %s", clientId);
            identity = null;
        }
        if (identity == null) {
            fail(context, "attestation verification failed");
            return;
        }

        // Store the verified identity for the Phase 2 mappers. Client notes on
        // the auth session propagate to the authenticated client session, which
        // is what protocol mappers read.
        AuthenticationSessionModel authSession = context.getSession().getContext().getAuthenticationSession();
        if (authSession != null) {
            authSession.setClientNote(NOTE_AGENT_SUBJECT, identity.subject());
            authSession.setClientNote(NOTE_AGENT_ID, identity.agentId());
            authSession.setClientNote(NOTE_AGENT_ISSUER, identity.issuer());
        }

        context.success();
    }

    private void fail(ClientAuthenticationFlowContext context, String detail) {
        // invalid_client, matching the C# validator's error surface. No detail
        // is leaked to the caller beyond the standard OAuth error description.
        Response challenge = ClientAuthUtil.errorResponse(
                Response.Status.UNAUTHORIZED.getStatusCode(),
                "invalid_client",
                "Attestation client authentication failed");
        log.debugf("spawnly-attestation auth failed: %s", detail);
        context.failure(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS, challenge);
    }

    // ── Factory (this class is its own factory, per AbstractClientAuthenticator) ──

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public String getDisplayType() {
        return "Spawnly Attestation (client_assertion)";
    }

    @Override
    public String getHelpText() {
        return "Authenticates machine clients with a SPIFFE JWT-SVID (or AWS STS) "
                + "attestation client_assertion, verified behind the ATTESTOR seam. "
                + "Non-attestation requests fall through to client-secret auth.";
    }

    @Override
    public boolean isConfigurable() {
        return false;
    }

    @Override
    public AuthenticationExecutionModel.Requirement[] getRequirementChoices() {
        return new AuthenticationExecutionModel.Requirement[]{
                AuthenticationExecutionModel.Requirement.ALTERNATIVE,
                AuthenticationExecutionModel.Requirement.DISABLED,
        };
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return Collections.emptyList();
    }

    @Override
    public List<ProviderConfigProperty> getConfigPropertiesPerClient() {
        return Collections.emptyList();
    }

    @Override
    public java.util.Map<String, Object> getAdapterConfiguration(ClientModel client) {
        // No keycloak-adapter (client-side) configuration: agents authenticate
        // with a runtime SPIFFE/STS credential, not a static adapter secret.
        return Collections.emptyMap();
    }

    @Override
    public Set<String> getProtocolAuthenticatorMethods(String loginProtocol) {
        if (OIDCLoginProtocol.LOGIN_PROTOCOL.equals(loginProtocol)) {
            // Advertise the two client_assertion types this authenticator handles.
            return Set.of(JWT_BEARER_ASSERTION_TYPE, AWS_STS_ASSERTION_TYPE);
        }
        return Collections.emptySet();
    }

    @Override
    public boolean supportsSecret() {
        // This authenticator does not use a client secret; the secret path is a
        // separate built-in authenticator running as the other ALTERNATIVE.
        return false;
    }
}
