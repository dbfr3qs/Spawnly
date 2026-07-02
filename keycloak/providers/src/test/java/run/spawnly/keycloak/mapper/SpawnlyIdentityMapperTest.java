package run.spawnly.keycloak.mapper;

import jakarta.ws.rs.core.MultivaluedHashMap;
import jakarta.ws.rs.core.MultivaluedMap;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.keycloak.http.HttpRequest;
import org.keycloak.models.ClientSessionContext;
import org.keycloak.models.KeycloakContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.ProtocolMapperModel;
import org.keycloak.models.UserSessionModel;
import org.keycloak.representations.AccessToken;

import run.spawnly.keycloak.identity.VerifiedAgent;

import java.util.Map;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertInstanceOf;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.when;

/**
 * The Phase 2 identity mapper: sub=user:&lt;userId&gt;, a NESTED act JSON object,
 * and token_use=delegation when audience=delegation. Reads the verified identity
 * from the request-scoped KeycloakSession attribute the client authenticator
 * stashed — and emits NOTHING when that attribute is absent (a non-attestation,
 * client-secret session), which is the security trust boundary.
 */
class SpawnlyIdentityMapperTest {

    private SpawnlyIdentityMapper mapper;
    private KeycloakSession session;
    private MultivaluedMap<String, String> form;
    private final ProtocolMapperModel model = SpawnlyIdentityMapper.create("id");
    private final UserSessionModel userSession = mock(UserSessionModel.class);
    private final ClientSessionContext csc = mock(ClientSessionContext.class);

    private static final String SPIFFE = "spiffe://cluster.local/agent/t/u/weather-monitor/agent-abc";

    @BeforeEach
    void setUp() {
        mapper = new SpawnlyIdentityMapper();
        session = mock(KeycloakSession.class);
        KeycloakContext kcCtx = mock(KeycloakContext.class);
        HttpRequest req = mock(HttpRequest.class);
        form = new MultivaluedHashMap<>();
        when(session.getContext()).thenReturn(kcCtx);
        when(kcCtx.getHttpRequest()).thenReturn(req);
        when(req.getDecodedFormParameters()).thenReturn(form);
    }

    private void verifiedAgentPresent() {
        when(session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class))
                .thenReturn(new VerifiedAgent("alice", SPIFFE, "agent-abc"));
    }

    @Test
    @SuppressWarnings("unchecked")
    void attestationSession_setsSubAndNestedAct() {
        verifiedAgentPresent();
        AccessToken token = new AccessToken();

        mapper.transformAccessToken(token, model, session, userSession, csc);

        assertEquals("user:alice", token.getSubject());
        Object act = token.getOtherClaims().get("act");
        // act MUST be a nested JSON object {"sub": "<spiffe>"}, not a string.
        assertInstanceOf(Map.class, act);
        assertEquals(SPIFFE, ((Map<String, Object>) act).get("sub"));
        // No delegation requested → no token_use.
        assertNull(token.getOtherClaims().get("token_use"));
    }

    @Test
    void audienceDelegation_setsTokenUse() {
        verifiedAgentPresent();
        form.putSingle("audience", "delegation");
        AccessToken token = new AccessToken();

        mapper.transformAccessToken(token, model, session, userSession, csc);

        assertEquals("delegation", token.getOtherClaims().get("token_use"));
        assertEquals("user:alice", token.getSubject());
    }

    @Test
    void audienceOtherThanDelegation_noTokenUse() {
        verifiedAgentPresent();
        form.putSingle("audience", "sample-api-a");
        AccessToken token = new AccessToken();

        mapper.transformAccessToken(token, model, session, userSession, csc);

        assertNull(token.getOtherClaims().get("token_use"));
    }

    @Test
    void noVerifiedAgent_emitsNothing_trustBoundary() {
        // A client-secret (human/control-plane) session: no VerifiedAgent stashed.
        when(session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class)).thenReturn(null);
        AccessToken token = new AccessToken();
        token.setSubject("some-uuid-sub"); // Keycloak's default sub

        mapper.transformAccessToken(token, model, session, userSession, csc);

        // Unchanged: sub NOT overridden, no act, no token_use.
        assertEquals("some-uuid-sub", token.getSubject());
        assertFalse(token.getOtherClaims().containsKey("act"));
        assertFalse(token.getOtherClaims().containsKey("token_use"));
    }

    @Test
    @SuppressWarnings("unchecked")
    void idToken_shapedIdentically() {
        verifiedAgentPresent();
        AccessToken idToken = new AccessToken(); // AccessToken extends IDToken

        mapper.transformIDToken(idToken, model, session, userSession, csc);

        assertEquals("user:alice", idToken.getSubject());
        assertInstanceOf(Map.class, idToken.getOtherClaims().get("act"));
        assertEquals(SPIFFE, ((Map<String, Object>) idToken.getOtherClaims().get("act")).get("sub"));
    }
}
