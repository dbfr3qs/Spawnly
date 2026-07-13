package run.spawnly.keycloak.registration;

import java.util.ArrayList;
import java.util.List;
import java.util.function.Supplier;

import org.jboss.logging.Logger;
import org.keycloak.authentication.FormAction;
import org.keycloak.authentication.FormContext;
import org.keycloak.authentication.ValidationContext;
import org.keycloak.events.Errors;
import org.keycloak.forms.login.LoginFormsProvider;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.models.UserModel;
import org.keycloak.models.utils.FormMessage;

/**
 * A registration-flow {@link FormAction} that enforces a global ceiling on the
 * number of realm users (signup-lifecycle Phase 2). When the realm already holds
 * {@code MAX_USERS} or more users, the self-serve registration form is rejected
 * with a friendly capacity message.
 *
 * <p><b>Why a registration-flow action (admin bypass).</b> This action runs ONLY
 * as an execution in the realm's registration flow, which is exercised by the
 * self-serve register page. Admin-API user creation (the adapter's
 * {@code createUser}, used by org onboarding / membership) does NOT run the
 * registration flow, so it naturally bypasses the cap — exactly the "admin
 * provisions beyond the cap" behaviour in the spec.
 *
 * <p><b>Fail-open on absence, fully validated value.</b> The cap is read from the
 * {@code MAX_USERS} env via an overridable supplier (so tests inject it). Unset,
 * empty, blank, non-numeric or non-positive all mean "no ceiling" — the action is
 * a no-op and registration proceeds unchanged, matching today's behaviour on a
 * cluster that never sets the var. Only a well-formed positive integer arms the
 * gate. This is the ONLY fail-open path and it fails open to today's (uncapped)
 * behaviour, never to a bypass of an ARMED cap.
 *
 * <p><b>Ordering / no partial user.</b> Keycloak runs every form action's
 * {@code validate()} before any {@code success()}; user creation happens in the
 * {@code success()} of {@code registration-user-creation}. So this action's
 * {@code getUsersCount} reads the PRE-creation count, and a rejection here (a
 * failed validate) stops the flow before any user is persisted — no orphaned
 * account is left behind. Single-replica safe: it reads a live count, holds no
 * clustered state ({@code --cache=local}).
 */
public class UserCapFormAction implements FormAction {

    public static final String PROVIDER_ID = "spawnly-user-cap";

    /** Env var carrying the ceiling. Unset/empty/non-positive ⇒ unlimited. */
    static final String ENV_MAX_USERS = "MAX_USERS";

    /**
     * Message key for the friendly capacity error, resolved from the {@code
     * spawnly} login theme's messages bundle (keycloak/themes/spawnly/login/
     * messages/messages_en.properties). A global ({@code field == null}) form
     * message so it renders in the register page's top alert banner.
     */
    static final String MSG_AT_CAPACITY = "spawnlySignupsAtCapacity";

    private static final Logger log = Logger.getLogger(UserCapFormAction.class);

    // Source of the raw MAX_USERS value. Production binds it to the process env;
    // tests inject a fixed supplier. Never null (the no-arg ctor supplies a
    // reader; the package-private ctor requires one).
    private final Supplier<String> capSource;

    /** Keycloak instantiates providers via a public no-arg constructor. */
    public UserCapFormAction() {
        this(() -> System.getenv(ENV_MAX_USERS));
    }

    /** Test seam: inject the raw cap source directly. */
    UserCapFormAction(Supplier<String> capSource) {
        this.capSource = capSource;
    }

    @Override
    public void validate(ValidationContext context) {
        int cap = configuredCap();
        if (cap <= 0) {
            // Unset / empty / non-positive / unparseable ⇒ no ceiling: no-op.
            context.success();
            return;
        }
        long current = context.getSession().users().getUsersCount(context.getRealm());
        if (current >= cap) {
            log.warnf("self-serve registration rejected: realm user count %d >= MAX_USERS %d",
                    current, cap);
            List<FormMessage> errors = new ArrayList<>();
            // field == null ⇒ a GLOBAL form message (top-of-page alert), not tied
            // to any register-form input.
            errors.add(new FormMessage(null, MSG_AT_CAPACITY));
            context.error(Errors.INVALID_REGISTRATION);
            context.validationError(context.getHttpRequest().getDecodedFormParameters(), errors);
            return;
        }
        context.success();
    }

    /**
     * The configured user ceiling, or a non-positive sentinel meaning "no cap".
     * An absent, blank, non-numeric or non-positive {@code MAX_USERS} all resolve
     * to 0 (unlimited); only a well-formed positive integer arms the gate.
     */
    private int configuredCap() {
        String raw = capSource.get();
        if (raw == null) {
            return 0;
        }
        raw = raw.trim();
        if (raw.isEmpty()) {
            return 0;
        }
        try {
            return Integer.parseInt(raw);
        } catch (NumberFormatException e) {
            log.warnf("ignoring non-numeric MAX_USERS=%s (treating as unlimited)", raw);
            return 0;
        }
    }

    @Override
    public void success(FormContext context) {
        // No-op: this action is a pure gate; the user is created by the separate
        // registration-user-creation form action.
    }

    @Override
    public void buildPage(FormContext context, LoginFormsProvider form) {
        // No-op: adds no fields to the registration page.
    }

    @Override
    public boolean requiresUser() {
        return false;
    }

    @Override
    public boolean configuredFor(KeycloakSession session, RealmModel realm, UserModel user) {
        return true;
    }

    @Override
    public void setRequiredActions(KeycloakSession session, RealmModel realm, UserModel user) {
        // No required actions to enqueue.
    }

    @Override
    public void close() {
    }
}
