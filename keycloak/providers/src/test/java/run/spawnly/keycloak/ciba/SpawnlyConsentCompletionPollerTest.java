package run.spawnly.keycloak.ciba;

import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.keycloak.models.KeycloakContext;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.RealmProvider;
import org.keycloak.protocol.oidc.grants.device.DeviceGrantType;
import org.mockito.MockedStatic;

import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.ConsentRequest;

import java.util.List;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.ArgumentMatchers.eq;
import static org.mockito.ArgumentMatchers.isNull;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.mockStatic;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.times;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * The completion poller — ports the C# ConsentCompletionPoller +
 * CibaCompletionService: for each tracked pending request it polls the registry
 * and, on approve, {@code approveUserCode}s the device code + records durable
 * consent; on deny, {@code denyUserCode}s it; pending is left for the next tick;
 * resolved entries are untracked; expired entries are swept.
 */
class SpawnlyConsentCompletionPollerTest {

    private KeycloakSession session;
    private RealmModel realm;
    private AgentRegistryClient registry;
    private ConsentRequestTracker tracker;

    @BeforeEach
    void setUp() {
        session = mock(KeycloakSession.class);
        realm = mock(RealmModel.class);
        registry = mock(AgentRegistryClient.class);
        tracker = new ConsentRequestTracker();

        KeycloakContext ctx = mock(KeycloakContext.class);
        RealmProvider realms = mock(RealmProvider.class);
        when(session.getContext()).thenReturn(ctx);
        when(session.realms()).thenReturn(realms);
        when(realms.getRealm("realm-1")).thenReturn(realm);
    }

    private ConsentRequestTracker.Entry track(String authResultId, long expiresAtMs) {
        ConsentRequestTracker.Entry e = new ConsentRequestTracker.Entry(
                "realm-1", authResultId, "cr-" + authResultId, "alice",
                "travel-planner", "flight-search", "flight-search",
                List.of("flights:read"), expiresAtMs);
        tracker.track(e);
        return e;
    }

    private ConsentRequest cr(String id, String status) {
        return new ConsentRequest(id, "alice", "travel-planner", "flight-search",
                "agent-child", List.of("flights:read"), null, status, "authres");
    }

    @Test
    void approve_approvesDeviceCode_recordsConsent_untracks() {
        track("authres-1", 0);
        when(registry.getConsentRequest("cr-authres-1")).thenReturn(cr("cr-authres-1", "approved"));
        when(registry.approveConsentRequest(eq("cr-authres-1"), anyList())).thenReturn(cr("cr-authres-1", "approved"));

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            dg.when(() -> DeviceGrantType.approveUserCode(eq(session), eq(realm), eq("authres-1"), any(), isNull()))
                    .thenReturn(true);

            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);

            dg.verify(() -> DeviceGrantType.approveUserCode(eq(session), eq(realm), eq("authres-1"), any(), isNull()));
        }
        verify(registry).approveConsentRequest(eq("cr-authres-1"), anyList());
        assertTrue(tracker.snapshot().isEmpty(), "approved entry must be untracked");
    }

    @Test
    void deny_deniesDeviceCode_untracks() {
        track("authres-2", 0);
        when(registry.getConsentRequest("cr-authres-2")).thenReturn(cr("cr-authres-2", "denied"));

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            dg.when(() -> DeviceGrantType.denyUserCode(eq(session), eq(realm), eq("authres-2"))).thenReturn(true);

            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);

            dg.verify(() -> DeviceGrantType.denyUserCode(eq(session), eq(realm), eq("authres-2")));
            dg.verify(() -> DeviceGrantType.approveUserCode(any(), any(), any(), any(), any()), never());
        }
        verify(registry, never()).approveConsentRequest(any(), anyList());
        assertTrue(tracker.snapshot().isEmpty());
    }

    @Test
    void pending_leftForNextTick() {
        track("authres-3", 0);
        when(registry.getConsentRequest("cr-authres-3")).thenReturn(cr("cr-authres-3", "pending"));

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);
            dg.verify(() -> DeviceGrantType.approveUserCode(any(), any(), any(), any(), any()), never());
            dg.verify(() -> DeviceGrantType.denyUserCode(any(), any(), any()), never());
        }
        assertEquals(1, tracker.snapshot().size(), "pending entry stays tracked");
    }

    @Test
    void registryUnreachable_leftForNextTick() {
        track("authres-4", 0);
        when(registry.getConsentRequest("cr-authres-4")).thenReturn(null); // fail closed

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);
            dg.verify(() -> DeviceGrantType.approveUserCode(any(), any(), any(), any(), any()), never());
        }
        assertEquals(1, tracker.snapshot().size());
    }

    @Test
    void expiredEntry_sweptBeforePolling() {
        long now = System.currentTimeMillis();
        track("authres-old", now - 1000); // already expired

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);
        }
        assertTrue(tracker.snapshot().isEmpty(), "expired entry swept");
        verify(registry, never()).getConsentRequest(any());
    }

    @Test
    void approve_whenDeviceCodeGone_stillUntracks() {
        // approveUserCode returns false (device code expired/redeemed) — the entry
        // is still untracked so it cannot leak.
        track("authres-5", 0);
        when(registry.getConsentRequest("cr-authres-5")).thenReturn(cr("cr-authres-5", "approved"));

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            dg.when(() -> DeviceGrantType.approveUserCode(any(), any(), any(), any(), any())).thenReturn(false);
            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);
        }
        assertTrue(tracker.snapshot().isEmpty());
    }

    @Test
    void multipleEntries_resolvedIndependently() {
        track("a", 0);
        track("b", 0);
        when(registry.getConsentRequest("cr-a")).thenReturn(cr("cr-a", "approved"));
        when(registry.getConsentRequest("cr-b")).thenReturn(cr("cr-b", "pending"));
        when(registry.approveConsentRequest(eq("cr-a"), anyList())).thenReturn(cr("cr-a", "approved"));

        try (MockedStatic<DeviceGrantType> dg = mockStatic(DeviceGrantType.class)) {
            dg.when(() -> DeviceGrantType.approveUserCode(any(), any(), eq("a"), any(), isNull())).thenReturn(true);
            new SpawnlyConsentCompletionPoller(registry, tracker).run(session);
            dg.verify(() -> DeviceGrantType.approveUserCode(any(), any(), eq("a"), any(), isNull()), times(1));
        }
        // a resolved (untracked), b still pending (tracked)
        List<String> remaining = tracker.snapshot().stream()
                .map(ConsentRequestTracker.Entry::authResultId).toList();
        assertEquals(List.of("b"), remaining);
    }
}
