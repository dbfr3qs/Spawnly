package run.spawnly.keycloak.authenticator;

import jakarta.ws.rs.core.MultivaluedHashMap;
import jakarta.ws.rs.core.MultivaluedMap;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.keycloak.authentication.AuthenticationFlowError;
import org.keycloak.authentication.ClientAuthenticationFlowContext;
import org.keycloak.events.EventBuilder;
import org.keycloak.http.HttpRequest;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.sessions.AuthenticationSessionModel;

import run.spawnly.keycloak.attestor.AgentCredentialVerifier;
import run.spawnly.keycloak.attestor.AgentIdentity;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * The custom client authenticator, mirroring
 * identityserver/AgentClientSecretValidator.cs: both attestation assertion
 * types are accepted (verified via the seam), an unknown/absent assertion type
 * falls through to the built-in secret path (attempted()), a missing client_id
 * is invalid_client, an unknown client is invalid_client, and on success the
 * verified {subject, agentId} land in the client-session notes for the Phase 2
 * mappers.
 */
class SpawnlyAttestationAuthenticatorTest {

    private ClientAuthenticationFlowContext ctx;
    private AuthenticationSessionModel authSession;
    private RealmModel realm;
    private MultivaluedMap<String, String> form;

    private static final String SPIFFE = "spiffe://cluster.local/agent/t/u/weather-monitor/agent-abc";

    @BeforeEach
    void setUp() {
        ctx = mock(ClientAuthenticationFlowContext.class);
        HttpRequest req = mock(HttpRequest.class);
        realm = mock(RealmModel.class);
        KeycloakSession session = mock(KeycloakSession.class);
        KeycloakContext kcCtx = mock(KeycloakContext.class);
        authSession = mock(AuthenticationSessionModel.class);
        EventBuilder event = mock(EventBuilder.class);

        form = new MultivaluedHashMap<>();
        when(ctx.getHttpRequest()).thenReturn(req);
        when(req.getDecodedFormParameters()).thenReturn(form);
        when(ctx.getRealm()).thenReturn(realm);
        when(ctx.getSession()).thenReturn(session);
        when(ctx.getEvent()).thenReturn(event);
        when(event.client(any(String.class))).thenReturn(event);
        when(session.getContext()).thenReturn(kcCtx);
        when(kcCtx.getAuthenticationSession()).thenReturn(authSession);
    }

    private SpawnlyAttestationAuthenticator authWithVerifier(AgentCredentialVerifier v) {
        return new SpawnlyAttestationAuthenticator(v);
    }

    private AgentCredentialVerifier alwaysOk() {
        AgentCredentialVerifier v = mock(AgentCredentialVerifier.class);
        when(v.verify(any())).thenReturn(new AgentIdentity("agent-abc", SPIFFE, "spiffe-svid"));
        return v;
    }

    @Test
    void jwtBearerAssertion_verifiedAndClientSet_notesStored() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        ClientModel client = mock(ClientModel.class);
        when(realm.getClientByClientId("weather-monitor")).thenReturn(client);

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).setClient(client);
        verify(ctx).success();
        verify(authSession).setClientNote(SpawnlyAttestationAuthenticator.NOTE_AGENT_SUBJECT, SPIFFE);
        verify(authSession).setClientNote(SpawnlyAttestationAuthenticator.NOTE_AGENT_ID, "agent-abc");
        verify(authSession).setClientNote(SpawnlyAttestationAuthenticator.NOTE_AGENT_ISSUER, "spiffe-svid");
    }

    @Test
    void awsStsAssertion_isAlsoAccepted() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.AWS_STS_ASSERTION_TYPE);
        form.putSingle("client_assertion", "presigned-sts-url");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).success();
    }

    @Test
    void unknownAssertionType_fallsThroughToSecretPath() {
        // A normal client_secret_jwt assertion type is not ours ⇒ attempted().
        form.putSingle("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:saml2-bearer");
        form.putSingle("client_assertion", "whatever");
        form.putSingle("client_id", "dashboard");

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).attempted();
        verify(ctx, never()).success();
        verify(ctx, never()).failure(any(), any());
    }

    @Test
    void noAssertion_fallsThroughToSecretPath() {
        // The human/control-plane client-secret request (no assertion at all).
        form.putSingle("client_id", "dashboard");
        form.putSingle("client_secret", "s3cret");

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).attempted();
    }

    @Test
    void missingClientId_isInvalidClient() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        // no client_id

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(ctx, never()).success();
    }

    @Test
    void unknownClient_isInvalidClient() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "ghost");
        when(realm.getClientByClientId("ghost")).thenReturn(null);

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
    }

    @Test
    void verificationFails_isInvalidClient_noNotes() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "bad-svid");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        AgentCredentialVerifier v = mock(AgentCredentialVerifier.class);
        when(v.verify(any())).thenReturn(null); // rejected credential

        authWithVerifier(v).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(authSession, never()).setClientNote(any(), any());
    }

    @Test
    void verifierThrows_isInvalidClient_notA500() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "explode");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        AgentCredentialVerifier v = mock(AgentCredentialVerifier.class);
        when(v.verify(any())).thenThrow(new RuntimeException("boom"));

        authWithVerifier(v).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
    }

    @Test
    void assertionTypeDetection() {
        org.junit.jupiter.api.Assertions.assertTrue(
                SpawnlyAttestationAuthenticator.isAttestationAssertionType(
                        SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE));
        org.junit.jupiter.api.Assertions.assertTrue(
                SpawnlyAttestationAuthenticator.isAttestationAssertionType(
                        SpawnlyAttestationAuthenticator.AWS_STS_ASSERTION_TYPE));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.isAttestationAssertionType("urn:something:else"));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.isAttestationAssertionType(null));
    }
}
