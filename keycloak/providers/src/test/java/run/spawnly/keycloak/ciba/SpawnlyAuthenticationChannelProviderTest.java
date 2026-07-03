package run.spawnly.keycloak.ciba;

import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.keycloak.models.ClientModel;
import org.keycloak.models.KeycloakContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.protocol.oidc.grants.ciba.channel.CIBAAuthenticationRequest;

import run.spawnly.keycloak.identity.VerifiedAgent;
import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.ConsentRequest;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * The CIBA authentication-channel provider — ports the C# CibaRequestValidator +
 * CibaConsentNotificationService semantics: the spawn edge is bound from the
 * VERIFIED SVID (the VerifiedAgent the attestation authenticator stashed), never
 * from request params; login_hint must match the agent's userId; parentless /
 * unknown-user agents are rejected; the registry consent request is created and
 * tracked; a covering stored consent (registry says approved) is tracked for
 * auto-approve.
 */
class SpawnlyAuthenticationChannelProviderTest {

    private static final String SPIFFE = "spiffe://cluster.local/agent/t/alice/flight-search/agent-child";

    private KeycloakSession session;
    private AgentRegistryClient registry;
    private ConsentRequestTracker tracker;
    private ClientModel client;

    @BeforeEach
    void setUp() {
        session = mock(KeycloakSession.class);
        registry = mock(AgentRegistryClient.class);
        tracker = new ConsentRequestTracker();
        client = mock(ClientModel.class);
        when(client.getClientId()).thenReturn("flight-search");

        KeycloakContext ctx = mock(KeycloakContext.class);
        RealmModel realm = mock(RealmModel.class);
        when(realm.getId()).thenReturn("realm-1");
        when(session.getContext()).thenReturn(ctx);
        when(ctx.getRealm()).thenReturn(realm);
    }

    private void verifiedAgentPresent() {
        when(session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class))
                .thenReturn(new VerifiedAgent("alice", SPIFFE, "agent-child"));
    }

    private CIBAAuthenticationRequest request() {
        CIBAAuthenticationRequest req = mock(CIBAAuthenticationRequest.class);
        when(req.getClient()).thenReturn(client);
        when(req.getScope()).thenReturn("openid flights:read");
        when(req.getBindingMessage()).thenReturn("flight-search requests flights:read");
        when(req.getAuthResultId()).thenReturn("authres-1");
        when(req.getExp()).thenReturn((System.currentTimeMillis() / 1000L) + 300);
        return req;
    }

    private SpawnlyAuthenticationChannelProvider provider() {
        return new SpawnlyAuthenticationChannelProvider(session, registry, tracker, false);
    }

    /** The standard edge: child agent-child (flight-search) under parent (travel-planner). */
    private void registryEdgeOk() {
        when(registry.getAgent("agent-child"))
                .thenReturn(new AgentRecord("agent-child", "awaiting-consent", "alice", "flight-search", "agent-parent"));
        when(registry.getAgent("agent-parent"))
                .thenReturn(new AgentRecord("agent-parent", "active", "alice", "travel-planner", null));
    }

    @Test
    void pending_createsAndTracksConsentRequest() {
        verifiedAgentPresent();
        registryEdgeOk();
        when(registry.createConsentRequest(eq("alice"), eq("travel-planner"), eq("flight-search"),
                anyList(), any(), eq("authres-1"), eq("agent-child")))
                .thenReturn(new ConsentRequest("cr-1", "alice", "travel-planner", "flight-search",
                        "agent-child", List.of("openid", "flights:read"), "msg",
                        ConsentRequest.STATUS_PENDING, "authres-1"));

        boolean ok = provider().requestAuthentication(request(), "alice");

        assertTrue(ok, "pending request must return true so the device code is stored");
        List<ConsentRequestTracker.Entry> tracked = tracker.snapshot();
        assertEquals(1, tracked.size());
        ConsentRequestTracker.Entry e = tracked.get(0);
        assertEquals("authres-1", e.authResultId());
        assertEquals("cr-1", e.registryRequestId());
        assertEquals("travel-planner", e.parentType());
        assertEquals("flight-search", e.childType());
        assertEquals("alice", e.userId());
    }

    @Test
    void storedConsent_approvedIsTrackedForAutoApprove() {
        verifiedAgentPresent();
        registryEdgeOk();
        when(registry.createConsentRequest(any(), any(), any(), anyList(), any(), any(), any()))
                .thenReturn(new ConsentRequest("cr-2", "alice", "travel-planner", "flight-search",
                        "agent-child", List.of("flights:read"), null,
                        ConsentRequest.STATUS_APPROVED, "authres-1"));

        boolean ok = provider().requestAuthentication(request(), "alice");

        assertTrue(ok);
        // Tracked (the poller approves the device code on its first tick).
        assertEquals(1, tracker.snapshot().size());
    }

    @Test
    void edgeBoundFromSvid_notFromLoginHint() {
        // The consent request MUST carry the userId + edge derived from the
        // VerifiedAgent (SVID) — never from the login_hint. Here the login_hint
        // matches; the point is the create call uses agent.userId/edge fields.
        verifiedAgentPresent();
        registryEdgeOk();
        when(registry.createConsentRequest(any(), any(), any(), anyList(), any(), any(), any()))
                .thenReturn(new ConsentRequest("cr", "alice", "travel-planner", "flight-search",
                        "agent-child", List.of(), null, ConsentRequest.STATUS_PENDING, "authres-1"));

        provider().requestAuthentication(request(), "alice");

        verify(registry).createConsentRequest(eq("alice"), eq("travel-planner"), eq("flight-search"),
                anyList(), any(), eq("authres-1"), eq("agent-child"));
    }

    @Test
    void noVerifiedAgent_rejected_whenDevApiOff() {
        // No attestation-authenticated identity ⇒ reject (client_assertion required).
        when(session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class)).thenReturn(null);

        boolean ok = provider().requestAuthentication(request(), "alice");

        assertFalse(ok);
        verify(registry, never()).createConsentRequest(any(), any(), any(), anyList(), any(), any(), any());
        assertTrue(tracker.snapshot().isEmpty());
    }

    @Test
    void loginHintMismatch_rejected() {
        // The login_hint cannot select an approver other than the agent's userId.
        verifiedAgentPresent();
        registryEdgeOk();

        boolean ok = provider().requestAuthentication(request(), "mallory");

        assertFalse(ok);
        verify(registry, never()).createConsentRequest(any(), any(), any(), anyList(), any(), any(), any());
    }

    @Test
    void unknownAgent_rejected() {
        verifiedAgentPresent();
        when(registry.getAgent("agent-child")).thenReturn(null);

        assertFalse(provider().requestAuthentication(request(), "alice"));
    }

    @Test
    void agentWithNoUser_rejected() {
        verifiedAgentPresent();
        when(registry.getAgent("agent-child"))
                .thenReturn(new AgentRecord("agent-child", "active", null, "flight-search", "agent-parent"));

        assertFalse(provider().requestAuthentication(request(), "alice"));
    }

    @Test
    void parentlessAgent_rejected() {
        // Consent gates a parent→child spawn edge; a root spawn is the user's own action.
        verifiedAgentPresent();
        when(registry.getAgent("agent-child"))
                .thenReturn(new AgentRecord("agent-child", "active", "alice", "flight-search", null));

        assertFalse(provider().requestAuthentication(request(), "alice"));
    }

    @Test
    void unknownParent_rejected() {
        verifiedAgentPresent();
        when(registry.getAgent("agent-child"))
                .thenReturn(new AgentRecord("agent-child", "active", "alice", "flight-search", "agent-parent"));
        when(registry.getAgent("agent-parent")).thenReturn(null);

        assertFalse(provider().requestAuthentication(request(), "alice"));
    }

    @Test
    void registryUnreachableOnCreate_leavesPendingNoTrack() {
        // Registry down at create time ⇒ still return true (sidecar gets an
        // auth_req_id + polls) but nothing to track (poller can't auto-complete).
        verifiedAgentPresent();
        registryEdgeOk();
        when(registry.createConsentRequest(any(), any(), any(), anyList(), any(), any(), any()))
                .thenReturn(null);

        boolean ok = provider().requestAuthentication(request(), "alice");

        assertTrue(ok);
        assertTrue(tracker.snapshot().isEmpty());
    }

    @Test
    void devApiFallback_acceptsWithoutVerifiedAgent() {
        when(session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class)).thenReturn(null);
        SpawnlyAuthenticationChannelProvider dev =
                new SpawnlyAuthenticationChannelProvider(session, registry, tracker, /* devCibaApi */ true);

        assertTrue(dev.requestAuthentication(request(), "alice"));
    }

    @Test
    void splitScopes() {
        assertEquals(List.of("openid", "flights:read"),
                SpawnlyAuthenticationChannelProvider.splitScopes("openid flights:read"));
        assertEquals(List.of(), SpawnlyAuthenticationChannelProvider.splitScopes(""));
        assertEquals(List.of(), SpawnlyAuthenticationChannelProvider.splitScopes(null));
        assertEquals(List.of("a"), SpawnlyAuthenticationChannelProvider.splitScopes("  a   a "));
    }
}
