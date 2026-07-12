package run.spawnly.keycloak.registration;

import jakarta.ws.rs.core.MultivaluedHashMap;
import jakarta.ws.rs.core.MultivaluedMap;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.keycloak.authentication.ValidationContext;
import org.keycloak.http.HttpRequest;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserProvider;
import org.keycloak.models.utils.FormMessage;
import org.mockito.ArgumentCaptor;

import java.util.List;
import java.util.function.Supplier;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyList;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * The global-user-cap registration form action (signup-lifecycle Phase 2). Under
 * the cap the form passes ({@code success()}); at or over the cap it fails with a
 * GLOBAL validation error carrying the friendly capacity message key; an
 * unset/empty/non-positive/non-numeric MAX_USERS is a no-op (unlimited). Modeled
 * on SpawnlyAttestationAuthenticatorTest's Mockito style.
 */
class UserCapFormActionTest {

    private ValidationContext ctx;
    private KeycloakSession session;
    private UserProvider users;
    private RealmModel realm;
    private MultivaluedMap<String, String> form;

    @BeforeEach
    void setUp() {
        ctx = mock(ValidationContext.class);
        session = mock(KeycloakSession.class);
        users = mock(UserProvider.class);
        realm = mock(RealmModel.class);
        HttpRequest req = mock(HttpRequest.class);
        form = new MultivaluedHashMap<>();

        when(ctx.getSession()).thenReturn(session);
        when(ctx.getRealm()).thenReturn(realm);
        when(ctx.getHttpRequest()).thenReturn(req);
        when(req.getDecodedFormParameters()).thenReturn(form);
        when(session.users()).thenReturn(users);
    }

    private UserCapFormAction actionWithCap(String rawCap) {
        Supplier<String> src = () -> rawCap;
        return new UserCapFormAction(src);
    }

    // ── Under the cap ───────────────────────────────────────────────────────────

    @Test
    void underCap_passes() {
        when(users.getUsersCount(realm)).thenReturn(5);

        actionWithCap("10").validate(ctx);

        verify(ctx).success();
        verify(ctx, never()).validationError(any(), anyList());
        verify(ctx, never()).error(any());
    }

    // ── At / over the cap ───────────────────────────────────────────────────────

    @Test
    void atCap_rejectedWithCapacityMessage() {
        when(users.getUsersCount(realm)).thenReturn(5);

        actionWithCap("5").validate(ctx);

        verify(ctx, never()).success();
        verify(ctx).error(any());

        @SuppressWarnings("unchecked")
        ArgumentCaptor<List<FormMessage>> errs = ArgumentCaptor.forClass(List.class);
        verify(ctx).validationError(any(), errs.capture());
        List<FormMessage> messages = errs.getValue();
        assertEquals(1, messages.size());
        // A GLOBAL message (no field) carrying the capacity key.
        assertEquals(UserCapFormAction.MSG_AT_CAPACITY, messages.get(0).getMessage());
    }

    @Test
    void overCap_rejected() {
        when(users.getUsersCount(realm)).thenReturn(9);

        actionWithCap("5").validate(ctx);

        verify(ctx, never()).success();
        verify(ctx).validationError(any(), anyList());
    }

    // ── Unset / malformed cap ⇒ no-op (unlimited) ───────────────────────────────

    @Test
    void unsetCap_isNoOp_countNotEvenRead() {
        actionWithCap(null).validate(ctx);

        verify(ctx).success();
        verify(ctx, never()).validationError(any(), anyList());
        // Fail-open must not even consult the store when there is no ceiling.
        verify(users, never()).getUsersCount(any());
    }

    @Test
    void emptyCap_isNoOp() {
        actionWithCap("   ").validate(ctx);

        verify(ctx).success();
        verify(ctx, never()).validationError(any(), anyList());
        verify(users, never()).getUsersCount(any());
    }

    @Test
    void zeroCap_isNoOp() {
        actionWithCap("0").validate(ctx);

        verify(ctx).success();
        verify(ctx, never()).validationError(any(), anyList());
    }

    @Test
    void negativeCap_isNoOp() {
        actionWithCap("-3").validate(ctx);

        verify(ctx).success();
        verify(ctx, never()).validationError(any(), anyList());
    }

    @Test
    void nonNumericCap_isNoOp() {
        actionWithCap("lots").validate(ctx);

        verify(ctx).success();
        verify(ctx, never()).validationError(any(), anyList());
    }

    // ── Boundary: cap of 1, empty realm allows exactly one signup ────────────────

    @Test
    void capOne_emptyRealm_passes() {
        when(users.getUsersCount(realm)).thenReturn(0);

        actionWithCap("1").validate(ctx);

        verify(ctx).success();
    }

    @Test
    void capOne_oneUser_rejected() {
        when(users.getUsersCount(realm)).thenReturn(1);

        actionWithCap("1").validate(ctx);

        verify(ctx, never()).success();
        verify(ctx).validationError(any(), anyList());
        assertTrue(true);
    }
}
