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
import run.spawnly.keycloak.attestor.OrgAwareVerifier;
import run.spawnly.keycloak.identity.VerifiedAgent;
import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;

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
 * Phase 5). On success it resolves the client by {@code client_id}, runs the
 * Phase 2 registry LIVENESS GATE (agent must be registered, {@code active}, have
 * a userId, and its registry {@code agentType} must equal the presented
 * {@code client_id}), and threads the registry-confirmed
 * {@code {userId, subject, agentId}} to the Phase 2 protocol mappers via the
 * per-request {@link org.keycloak.models.KeycloakSession} attribute bag (see
 * {@link run.spawnly.keycloak.identity.VerifiedAgent} for why a session
 * attribute, not a client note, is the reliable channel on client_credentials).
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

    // Per-org verifier resolution (Phase 9): the platform default verifier
    // (selected once from ATTESTOR) wrapped so an STS credential resolves to the
    // anchor of the org owning the agent's record — the isolation invariant. A
    // non-STS (SPIRE) credential is passed straight to the default, so the SPIRE
    // path is bit-identical. AbstractClientAuthenticator uses ONE instance as both
    // the factory and the authenticator (create() returns this), so this is a
    // mutable field populated in init(). An unknown ATTESTOR makes init() throw —
    // a HARD FAIL at server startup, matching Program.cs.
    private volatile OrgAwareVerifier verifier;

    // The registry client is built once at factory init and shared (its HTTP
    // client + bearer supplier are thread-safe). Used by the Phase 2 liveness
    // gate. Injected in tests via the package-private ctor.
    private volatile AgentRegistryClient registry;

    /** Keycloak instantiates providers via a public no-arg constructor. */
    public SpawnlyAttestationAuthenticator() {
    }

    /** Test seam: inject a base verifier directly, bypassing ATTESTOR selection. */
    SpawnlyAttestationAuthenticator(AgentCredentialVerifier verifier) {
        this.verifier = new OrgAwareVerifier(verifier, null, "");
    }

    /** Test seam: inject both the base verifier and the registry client. */
    SpawnlyAttestationAuthenticator(AgentCredentialVerifier verifier, AgentRegistryClient registry) {
        this.verifier = new OrgAwareVerifier(verifier, registry, "");
        this.registry = registry;
    }

    /** Test seam: inject a fully-built org-aware verifier (for the STS path). */
    SpawnlyAttestationAuthenticator(OrgAwareVerifier verifier, AgentRegistryClient registry) {
        this.verifier = verifier;
        this.registry = registry;
    }

    @Override
    public void init(org.keycloak.Config.Scope config) {
        // Select the attestor once. Unknown value ⇒ IllegalStateException ⇒
        // startup fails loudly (no silent insecure fallback). Wrap it in the
        // org-aware resolver, built with the STS_TRUSTED_ENDPOINT override so the
        // org-bound STS verifier honours the fake STS in e2e.
        this.registry = new AgentRegistryClient();
        this.verifier = new OrgAwareVerifier(
                AttestorFactory.build(config), registry, AttestorFactory.resolveStsTrustedEndpoint(config));
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

        // Per-org verifier resolution (Phase 9): for an STS credential this binds
        // the AWS-attested account+ARN to the anchor of the org owning the agent's
        // record (the isolation invariant); a SPIRE credential goes to the platform
        // default unchanged. Any failure — bad credential, unknown record, wrong
        // org, unanchored org, registry outage — returns null, which becomes the
        // SAME invalid_client below (oracle-free).
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

        // ── Registry liveness gate (Phase 2 + Phase 4 grant-aware) ─────────────
        // Port of AgentRegistryValidator.cs: look the agent up by its derived
        // agentId, require it to be registered with an allowed status, and
        // require a userId (the human it acts for). A missing agent, wrong
        // status, or missing userId is invalid_client — the same error surface as
        // the C# validator.
        //
        // GRANT-AWARENESS (Phase 4, AgentRegistryValidator.cs:45): the CIBA paths
        // — the backchannel auth request and the CIBA token poll — run BEFORE the
        // agent is fully active (the sidecar drives consent at spawn), so they
        // accept active|pending|awaiting-consent. The client_credentials mint
        // keeps requiring `active`. So a REVOKED agent can never redeem an
        // approved auth_req_id (revoked ∉ the allowed set on any path) — real-time
        // revocation still starves an approved-but-unredeemed request.
        boolean cibaPath = isCibaPath(context, params);
        AgentRecord agent = registry.getAgent(identity.agentId());
        if (agent == null || !statusAllowed(agent.status(), cibaPath)) {
            fail(context, "agent " + identity.agentId() + " status not allowed"
                    + (cibaPath ? " (ciba)" : " (client_credentials)"));
            return;
        }
        if (agent.userId() == null || agent.userId().isEmpty()) {
            fail(context, "agent " + identity.agentId() + " has no userId");
            return;
        }

        // ── Phase-1 follow-up (b): bind the SVID to the client ────────────────
        // The platform models an agent's TYPE as its Keycloak client_id (a
        // machine client is named `weather-monitor`, `chain-worker`, ...). The
        // registry independently records the agent's agentType from its own
        // SVID-based registration. Require the two to match, so an SVID for
        // agent-type X cannot authenticate as a different client Y and inherit
        // Y's scopes. The registry record is authoritative for agentType (the
        // SVID path position varies between tenanted and global agents), so we
        // compare agent.agentType() to the presented client_id.
        if (!clientId.equals(agent.agentType())) {
            fail(context, "agent " + identity.agentId() + " type " + agent.agentType()
                    + " does not match client_id " + clientId);
            return;
        }

        // Thread the registry-confirmed identity to the Phase 2 mappers via the
        // per-request KeycloakSession attribute bag — the RELIABLE channel on the
        // client_credentials grant (see VerifiedAgent for why client notes don't
        // survive that grant). sub=user:<userId> + act.sub=<spiffe> + the
        // organization claim are derived from THIS object, so they can never be
        // client-supplied. orgId is the REGISTRY record's org (multi-tenancy),
        // binding the token's org claim to what was recorded at spawn.
        VerifiedAgent verified = new VerifiedAgent(
                agent.userId(), identity.subject(), identity.agentId(), agent.orgId());
        context.getSession().setAttribute(VerifiedAgent.SESSION_ATTR, verified);

        // Also keep the client notes for callers/paths that DO reuse the auth
        // session (e.g. the Phase 4 CIBA channel), and for audit. Harmless where
        // they don't propagate.
        AuthenticationSessionModel authSession = context.getSession().getContext().getAuthenticationSession();
        if (authSession != null) {
            authSession.setClientNote(NOTE_AGENT_SUBJECT, identity.subject());
            authSession.setClientNote(NOTE_AGENT_ID, identity.agentId());
            authSession.setClientNote(NOTE_AGENT_ISSUER, identity.issuer());
        }

        context.success();
    }

    // ── Grant-aware liveness (Phase 4) ──────────────────────────────────────────

    // The CIBA token-poll grant type (OAuth2Constants.CIBA_GRANT_TYPE on this KC).
    static final String CIBA_GRANT_TYPE = "urn:openid:params:grant-type:ciba";
    // The backchannel authentication endpoint path suffix (no grant_type param).
    static final String CIBA_AUTH_PATH_SUFFIX = "/ext/ciba/auth";

    /** Statuses acceptable to open/redeem a CIBA request; the sidecar runs CIBA
     * before the agent is fully active. Revoked/terminal is NOT here. */
    private static final Set<String> CIBA_ALLOWED_STATUSES =
            Set.of("active", "pending", "awaiting-consent");

    /**
     * True when this client-auth is for a CIBA path: either the CIBA token-poll
     * grant (grant_type=urn:openid:params:grant-type:ciba) or the backchannel
     * auth request endpoint (…/ext/ciba/auth, which carries no grant_type). On
     * both, the wider status set applies; every other path requires `active`.
     */
    static boolean isCibaPath(ClientAuthenticationFlowContext context, MultivaluedMap<String, String> params) {
        String grantType = params.getFirst(OAuth2Constants.GRANT_TYPE);
        if (CIBA_GRANT_TYPE.equals(grantType)) {
            return true;
        }
        try {
            String path = context.getHttpRequest().getUri().getPath();
            return path != null && path.endsWith(CIBA_AUTH_PATH_SUFFIX);
        } catch (RuntimeException e) {
            // No URI context ⇒ treat as a non-CIBA path (fail closed to `active`).
            return false;
        }
    }

    /** Whether {@code status} is acceptable on this path (client_credentials → active only). */
    static boolean statusAllowed(String status, boolean cibaPath) {
        if (cibaPath) {
            return status != null && CIBA_ALLOWED_STATUSES.contains(status);
        }
        return "active".equals(status);
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
