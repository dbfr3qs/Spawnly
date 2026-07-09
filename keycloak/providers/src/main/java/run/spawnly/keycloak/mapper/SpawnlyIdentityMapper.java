package run.spawnly.keycloak.mapper;

import jakarta.ws.rs.core.MultivaluedMap;
import org.jboss.logging.Logger;
import org.keycloak.models.ClientSessionContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ProtocolMapperModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.protocol.ProtocolMapperUtils;
import org.keycloak.protocol.oidc.mappers.AbstractOIDCProtocolMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAccessTokenMapper;
import org.keycloak.protocol.oidc.mappers.OIDCAttributeMapperHelper;
import org.keycloak.protocol.oidc.mappers.OIDCIDTokenMapper;
import org.keycloak.protocol.oidc.mappers.UserInfoTokenMapper;
import org.keycloak.provider.ProviderConfigProperty;
import org.keycloak.representations.AccessToken;
import org.keycloak.representations.IDToken;

import run.spawnly.keycloak.identity.VerifiedAgent;

import java.util.Collections;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/**
 * The Phase 2 registry-driven identity mapper. Ported from the token-shaping
 * half of identityserver/AgentRegistryValidator.cs: on a machine
 * {@code client_credentials} token it reshapes the token to the platform's
 * delegated-identity shape so downstream resource servers stay unchanged:
 *
 * <ul>
 *   <li>{@code sub = user:<userId>} — the human principal the agent acts for;</li>
 *   <li>{@code act = {"sub":"<spiffe URI>"}} — a NESTED JSON object (the agent is
 *       the actor); and</li>
 *   <li>{@code token_use = delegation} when the token request carried
 *       {@code audience=delegation} (a delegation-only token a parent hands to a
 *       child as the subject_token of a later exchange).</li>
 * </ul>
 *
 * <p>SECURITY: every value emitted here derives from the {@link VerifiedAgent}
 * the client authenticator stashed AFTER verifying the SVID and passing the
 * registry liveness gate. If no VerifiedAgent is present (i.e. this was NOT an
 * attestation-authenticated request — e.g. a client-secret human/control-plane
 * client), the mapper emits NOTHING. So the sub/act override can never fire for
 * a client-secret client, and {@code sub}/{@code act} can never be
 * client-supplied. This mapper never emits {@code role} — that stays a
 * user-sourced claim, attached only to dashboard/mobile (scopes.tf).
 *
 * <p>Attached via Terraform to the machine clients only (deploy/keycloak).
 */
public class SpawnlyIdentityMapper extends AbstractOIDCProtocolMapper
        implements OIDCAccessTokenMapper, OIDCIDTokenMapper, UserInfoTokenMapper {

    public static final String PROVIDER_ID = "spawnly-identity-mapper";
    private static final Logger log = Logger.getLogger(SpawnlyIdentityMapper.class);

    private static final String ACT_CLAIM = "act";
    private static final String TOKEN_USE_CLAIM = "token_use";
    private static final String ORGANIZATION_CLAIM = "organization";
    private static final String DELEGATION = "delegation";
    private static final String AUDIENCE_PARAM = "audience";

    private static final List<ProviderConfigProperty> CONFIG = Collections.emptyList();

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public String getDisplayType() {
        return "Spawnly Agent Identity (sub/act/token_use)";
    }

    @Override
    public String getHelpText() {
        return "Sets sub=user:<userId>, a nested act={\"sub\":\"<spiffe>\"} actor claim, "
                + "and token_use=delegation for attestation-authenticated machine tokens. "
                + "Emits nothing for non-attestation (client-secret) sessions.";
    }

    @Override
    public String getDisplayCategory() {
        return TOKEN_MAPPER_CATEGORY;
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return CONFIG;
    }

    @Override
    public AccessToken transformAccessToken(AccessToken token, ProtocolMapperModel model,
                                            KeycloakSession session, UserSessionModel userSession,
                                            ClientSessionContext clientSessionCtx) {
        apply(token, session);
        return token;
    }

    @Override
    public IDToken transformIDToken(IDToken token, ProtocolMapperModel model,
                                    KeycloakSession session, UserSessionModel userSession,
                                    ClientSessionContext clientSessionCtx) {
        // The client_credentials access token is the security-relevant one, but
        // shaping the id_token identically (when present) keeps the identity
        // consistent and matches the C# claim placement.
        apply(token, session);
        return token;
    }

    /**
     * Applies the sub/act/token_use shaping to any {@link IDToken} (the common
     * supertype of AccessToken/IDToken). No-op unless the request is
     * attestation-authenticated (a {@link VerifiedAgent} is present).
     */
    private void apply(IDToken token, KeycloakSession session) {
        VerifiedAgent agent = session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class);
        if (agent == null) {
            // Not an attestation-authenticated request → emit nothing. This is
            // the trust boundary: a client-secret client never populates the
            // session attribute, so its token is never reshaped here.
            return;
        }

        // sub = user:<userId>. Overrides Keycloak's default service-account sub.
        token.setSubject("user:" + agent.userId());

        // act = { "sub": "<spiffe URI>" } — a NESTED JSON object. Keycloak
        // serializes a Map value in otherClaims as a JSON object, reproducing the
        // C# JsonClaimValueTypes.Json shape.
        Map<String, Object> act = new LinkedHashMap<>();
        act.put("sub", agent.subject());
        token.setOtherClaims(ACT_CLAIM, act);

        // organization = ["<orgId>"] — the SAME array-of-aliases shape Keycloak's
        // org membership mapper stamps into USER tokens (frozen live-captured
        // shape; see internal/tokenvalidator/keycloak_fixture_test.go), so the Go
        // tokenvalidator parses agent + user tokens identically. Emitted ONLY for
        // an org-bound agent; an org-less (global) agent carries NO org claim.
        // The value is the REGISTRY record's orgId (threaded via VerifiedAgent),
        // so an agent can never mint a token claiming an org other than the one
        // recorded at spawn.
        String orgId = agent.orgId();
        if (orgId != null && !orgId.isEmpty()) {
            token.setOtherClaims(ORGANIZATION_CLAIM, List.of(orgId));
        }

        // token_use=delegation when the token request carried audience=delegation.
        if (isDelegationRequested(session)) {
            token.setOtherClaims(TOKEN_USE_CLAIM, DELEGATION);
        }
    }

    /**
     * True when the current token request carries {@code audience=delegation}.
     * Read from the decoded form parameters of the request that is minting this
     * token — the same place the C# reads {@code request.Raw.Get("audience")}.
     */
    private boolean isDelegationRequested(KeycloakSession session) {
        try {
            MultivaluedMap<String, String> form =
                    session.getContext().getHttpRequest().getDecodedFormParameters();
            return form != null && DELEGATION.equals(form.getFirst(AUDIENCE_PARAM));
        } catch (RuntimeException e) {
            // No form context (e.g. userinfo/refresh) ⇒ not a delegation mint.
            log.debugf(e, "could not read audience param; treating as non-delegation");
            return false;
        }
    }

    /**
     * Builds a ProtocolMapperModel for programmatic attachment/testing. The
     * mapper has no per-instance config; it always reads the session attribute.
     */
    public static ProtocolMapperModel create(String name) {
        ProtocolMapperModel model = new ProtocolMapperModel();
        model.setName(name);
        model.setProtocolMapper(PROVIDER_ID);
        model.setProtocol("openid-connect");
        Map<String, String> config = new LinkedHashMap<>();
        config.put(OIDCAttributeMapperHelper.INCLUDE_IN_ACCESS_TOKEN, "true");
        config.put(OIDCAttributeMapperHelper.INCLUDE_IN_ID_TOKEN, "true");
        config.put(ProtocolMapperUtils.MULTIVALUED, "false");
        model.setConfig(config);
        return model;
    }
}
