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
import run.spawnly.keycloak.attestor.OrgAwareVerifier;
import run.spawnly.keycloak.identity.VerifiedAgent;
import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.AttestationAnchor;
import run.spawnly.keycloak.registry.Organization;

import java.nio.charset.StandardCharsets;
import java.util.Base64;

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
    private KeycloakSession session;
    private AgentRegistryClient registry;
    private MultivaluedMap<String, String> form;

    private static final String SPIFFE = "spiffe://cluster.local/agent/t/u/weather-monitor/agent-abc";

    @BeforeEach
    void setUp() {
        ctx = mock(ClientAuthenticationFlowContext.class);
        HttpRequest req = mock(HttpRequest.class);
        realm = mock(RealmModel.class);
        session = mock(KeycloakSession.class);
        KeycloakContext kcCtx = mock(KeycloakContext.class);
        authSession = mock(AuthenticationSessionModel.class);
        EventBuilder event = mock(EventBuilder.class);
        registry = mock(AgentRegistryClient.class);

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
        return new SpawnlyAttestationAuthenticator(v, registry);
    }

    private AgentCredentialVerifier alwaysOk() {
        AgentCredentialVerifier v = mock(AgentCredentialVerifier.class);
        when(v.verify(any())).thenReturn(new AgentIdentity("agent-abc", SPIFFE, "spiffe-svid"));
        return v;
    }

    /** Registry returns an ACTIVE org-less agent whose agentType matches the client_id. */
    private void registryReturnsActive(String clientId) {
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "active", "alice", clientId, null));
    }

    /** Registry returns an ACTIVE agent belonging to org {@code orgId}. */
    private void registryReturnsActiveInOrg(String clientId, String orgId) {
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "active", "alice", clientId, null, orgId));
    }

    @Test
    void jwtBearerAssertion_verifiedAndClientSet_notesStored() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        ClientModel client = mock(ClientModel.class);
        when(realm.getClientByClientId("weather-monitor")).thenReturn(client);
        registryReturnsActive("weather-monitor");

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).setClient(client);
        verify(ctx).success();
        // The reliable channel to the Phase 2 mappers: the verified, registry-
        // confirmed identity in the request-scoped session attribute bag. Org-less
        // record ⇒ orgId=null on the VerifiedAgent.
        verify(session).setAttribute(VerifiedAgent.SESSION_ATTR,
                new VerifiedAgent("alice", SPIFFE, "agent-abc", null));
        // Client notes still set (harmless where they don't propagate; used by CIBA).
        verify(authSession).setClientNote(SpawnlyAttestationAuthenticator.NOTE_AGENT_SUBJECT, SPIFFE);
        verify(authSession).setClientNote(SpawnlyAttestationAuthenticator.NOTE_AGENT_ID, "agent-abc");
        verify(authSession).setClientNote(SpawnlyAttestationAuthenticator.NOTE_AGENT_ISSUER, "spiffe-svid");
    }

    @Test
    void awsStsAssertion_assertionTypeIsAccepted() {
        // The AWS_STS assertion TYPE is accepted (not fallen through to the secret
        // path). This payload is not a real base64 presigned envelope, so Phase 9's
        // OrgAwareVerifier routes it through the default branch (the injected mock);
        // the real org-bound STS path is covered by stsAnchoredOrgAgent_* below and
        // OrgAwareVerifierTest.
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.AWS_STS_ASSERTION_TYPE);
        form.putSingle("client_assertion", "presigned-sts-url");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        registryReturnsActive("weather-monitor");

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).success();
    }

    // ── Registry liveness gate matrix (Phase 2) ────────────────────────────────

    @Test
    void livenessGate_missingAgent_isInvalidClient_noAttribute() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("agent-abc")).thenReturn(null); // not registered / registry down

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(ctx, never()).success();
        verify(session, never()).setAttribute(eq(VerifiedAgent.SESSION_ATTR), any());
    }

    @Test
    void livenessGate_revokedAgent_isInvalidClient() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "revoked", "alice", "weather-monitor", null));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(ctx, never()).success();
    }

    @Test
    void livenessGate_pendingStatus_rejectedOnClientCredentials() {
        // Phase 4 will allow pending on the CIBA path, but the client_credentials
        // gate requires strictly `active` (AgentRegistryValidator.cs).
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "pending", "alice", "weather-monitor", null));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
    }

    @Test
    void livenessGate_noUserId_isInvalidClient() {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "active", null, "weather-monitor", null));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
    }

    @Test
    void livenessGate_agentTypeMismatch_isInvalidClient() {
        // Phase-1 follow-up (b): an SVID for type weather-monitor must not be
        // usable to authenticate as a DIFFERENT client (chain-worker) and inherit
        // its scopes. The registry agentType must equal the presented client_id.
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "chain-worker");
        when(realm.getClientByClientId("chain-worker")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "active", "alice", "weather-monitor", null));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(ctx, never()).success();
        verify(session, never()).setAttribute(eq(VerifiedAgent.SESSION_ATTR), any());
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

    // ── Grant-aware liveness matrix (Phase 4) ──────────────────────────────────
    // On the CIBA paths (grant_type=urn:openid:params:grant-type:ciba, or the
    // …/ext/ciba/auth backchannel endpoint) the sidecar runs CIBA before the
    // agent is fully active, so active|pending|awaiting-consent are accepted; a
    // revoked agent is rejected on EVERY path (so it can never redeem an approved
    // auth_req_id — real-time revocation starves it).

    private void cibaGrantForm(String clientId) {
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", clientId);
        form.putSingle("grant_type", SpawnlyAttestationAuthenticator.CIBA_GRANT_TYPE);
        when(realm.getClientByClientId(clientId)).thenReturn(mock(ClientModel.class));
    }

    @Test
    void cibaPath_pendingStatus_allowed() {
        cibaGrantForm("chain-worker");
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "pending", "alice", "chain-worker", "parent-1"));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).success();
        verify(session).setAttribute(VerifiedAgent.SESSION_ATTR,
                new VerifiedAgent("alice", SPIFFE, "agent-abc", null));
    }

    @Test
    void orgAgent_orgIdThreadedFromRegistryRecord() {
        // Multi-tenancy: the registry record's orgId is threaded into the
        // VerifiedAgent (→ the token's organization claim), registry-bound so an
        // agent can never claim an org other than the one recorded at spawn.
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.JWT_BEARER_ASSERTION_TYPE);
        form.putSingle("client_assertion", "the-svid");
        form.putSingle("client_id", "weather-monitor");
        when(realm.getClientByClientId("weather-monitor")).thenReturn(mock(ClientModel.class));
        registryReturnsActiveInOrg("weather-monitor", "acme");

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).success();
        verify(session).setAttribute(VerifiedAgent.SESSION_ATTR,
                new VerifiedAgent("alice", SPIFFE, "agent-abc", "acme"));
    }

    @Test
    void stsAnchoredOrgAgent_mintsOrgBound_orgStamped() {
        // Phase 9 end-to-end: an STS credential for an agent recorded under an
        // anchored org verifies org-bound (account+role match the org anchor) and
        // its orgId is threaded into the VerifiedAgent — mapper/exchange org
        // stamping is re-asserted for an anchored-org agent.
        String arn = "arn:aws:sts::111122223333:assumed-role/acme-agents/remote-1";
        String stsCred = Base64.getEncoder().encodeToString(
                "{\"method\":\"POST\",\"url\":\"https://sts.amazonaws.com/?Action=GetCallerIdentity\"}"
                        .getBytes(StandardCharsets.UTF_8));
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.AWS_STS_ASSERTION_TYPE);
        form.putSingle("client_assertion", stsCred);
        form.putSingle("client_id", "remote-worker");
        when(realm.getClientByClientId("remote-worker")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("remote-1"))
                .thenReturn(new AgentRecord("remote-1", "active", "alice", "remote-worker", null, "acme"));
        when(registry.getOrg("acme")).thenReturn(new Organization("acme", "acme",
                new AttestationAnchor("sts", "111122223333",
                        "arn:aws:sts::111122223333:assumed-role/acme-agents/*", "active")));

        // The STS replay is mocked to attest the acme identity; the org-bound
        // binding (account + role) is the real code under test.
        AgentCredentialVerifier stsReplay = mock(AgentCredentialVerifier.class);
        when(stsReplay.verify(any())).thenReturn(new AgentIdentity("remote-1", arn, "aws-sts"));
        OrgAwareVerifier orgAware = new OrgAwareVerifier(mock(AgentCredentialVerifier.class), stsReplay, registry);

        new SpawnlyAttestationAuthenticator(orgAware, registry).authenticateClient(ctx);

        verify(ctx).success();
        verify(session).setAttribute(VerifiedAgent.SESSION_ATTR,
                new VerifiedAgent("alice", arn, "remote-1", "acme"));
    }

    @Test
    void stsCrossOrgCredential_isInvalidClient() {
        // Isolation: an acme STS credential presented for an agent recorded under
        // globex is refused — org A's account can never authenticate an org B agent.
        String acmeArn = "arn:aws:sts::111122223333:assumed-role/acme-agents/remote-1";
        String stsCred = Base64.getEncoder().encodeToString(
                "{\"method\":\"POST\",\"url\":\"https://sts.amazonaws.com/?Action=GetCallerIdentity\"}"
                        .getBytes(StandardCharsets.UTF_8));
        form.putSingle("client_assertion_type", SpawnlyAttestationAuthenticator.AWS_STS_ASSERTION_TYPE);
        form.putSingle("client_assertion", stsCred);
        form.putSingle("client_id", "remote-worker");
        when(realm.getClientByClientId("remote-worker")).thenReturn(mock(ClientModel.class));
        when(registry.getAgent("remote-1"))
                .thenReturn(new AgentRecord("remote-1", "active", "alice", "remote-worker", null, "globex"));
        when(registry.getOrg("globex")).thenReturn(new Organization("globex", "globex",
                new AttestationAnchor("sts", "999988887777",
                        "arn:aws:sts::999988887777:assumed-role/globex-agents/*", "active")));

        AgentCredentialVerifier stsReplay = mock(AgentCredentialVerifier.class);
        when(stsReplay.verify(any())).thenReturn(new AgentIdentity("remote-1", acmeArn, "aws-sts"));
        OrgAwareVerifier orgAware = new OrgAwareVerifier(mock(AgentCredentialVerifier.class), stsReplay, registry);

        new SpawnlyAttestationAuthenticator(orgAware, registry).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(ctx, never()).success();
        verify(session, never()).setAttribute(eq(VerifiedAgent.SESSION_ATTR), any());
    }

    @Test
    void cibaPath_awaitingConsentStatus_allowed() {
        cibaGrantForm("chain-worker");
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "awaiting-consent", "alice", "chain-worker", "parent-1"));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).success();
    }

    @Test
    void cibaPath_activeStatus_allowed() {
        cibaGrantForm("chain-worker");
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "active", "alice", "chain-worker", "parent-1"));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).success();
    }

    @Test
    void cibaPath_revokedStatus_rejected_cannotRedeem() {
        // The security core of revocation-starves-CIBA: a revoked agent cannot
        // redeem an approved auth_req_id because it can't even authenticate.
        cibaGrantForm("chain-worker");
        when(registry.getAgent("agent-abc"))
                .thenReturn(new AgentRecord("agent-abc", "revoked", "alice", "chain-worker", "parent-1"));

        authWithVerifier(alwaysOk()).authenticateClient(ctx);

        verify(ctx).failure(eq(AuthenticationFlowError.INVALID_CLIENT_CREDENTIALS), any());
        verify(ctx, never()).success();
        verify(session, never()).setAttribute(eq(VerifiedAgent.SESSION_ATTR), any());
    }

    @Test
    void statusAllowed_matrix() {
        // client_credentials: only active
        org.junit.jupiter.api.Assertions.assertTrue(
                SpawnlyAttestationAuthenticator.statusAllowed("active", false));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.statusAllowed("pending", false));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.statusAllowed("awaiting-consent", false));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.statusAllowed("revoked", false));
        // ciba: active|pending|awaiting-consent, NOT revoked
        org.junit.jupiter.api.Assertions.assertTrue(
                SpawnlyAttestationAuthenticator.statusAllowed("active", true));
        org.junit.jupiter.api.Assertions.assertTrue(
                SpawnlyAttestationAuthenticator.statusAllowed("pending", true));
        org.junit.jupiter.api.Assertions.assertTrue(
                SpawnlyAttestationAuthenticator.statusAllowed("awaiting-consent", true));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.statusAllowed("revoked", true));
        org.junit.jupiter.api.Assertions.assertFalse(
                SpawnlyAttestationAuthenticator.statusAllowed(null, true));
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
