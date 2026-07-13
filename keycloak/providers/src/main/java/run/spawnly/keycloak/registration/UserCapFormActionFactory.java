package run.spawnly.keycloak.registration;

import java.util.Collections;
import java.util.List;

import org.keycloak.Config;
import org.keycloak.authentication.FormAction;
import org.keycloak.authentication.FormActionFactory;
import org.keycloak.models.AuthenticationExecutionModel.Requirement;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.provider.ProviderConfigProperty;

/**
 * Factory for {@link UserCapFormAction}, registered via
 * META-INF/services/org.keycloak.authentication.FormActionFactory so the
 * {@code spawnly-user-cap} form action can be added as an execution to the
 * realm's registration flow (see deploy/keycloak/registration-flow.tf).
 *
 * <p>The action is stateless (its only input is the live user count + the
 * MAX_USERS env), so a single shared instance is returned per request — the same
 * idiom Keycloak's own built-in registration form actions use
 * ({@code RegistrationUserCreation.SINGLETON}).
 */
public class UserCapFormActionFactory implements FormActionFactory {

    // Stateless action → one shared instance (mirrors the built-in
    // registration form-action factories).
    private static final UserCapFormAction SINGLETON = new UserCapFormAction();

    // REQUIRED so it actually gates the flow, or DISABLED to turn it off; the
    // cap itself is armed by the MAX_USERS env, not by an ALTERNATIVE toggle.
    private static final Requirement[] REQUIREMENT_CHOICES = {
            Requirement.REQUIRED,
            Requirement.DISABLED,
    };

    @Override
    public String getId() {
        return UserCapFormAction.PROVIDER_ID;
    }

    @Override
    public FormAction create(KeycloakSession session) {
        return SINGLETON;
    }

    @Override
    public void init(Config.Scope config) {
    }

    @Override
    public void postInit(KeycloakSessionFactory factory) {
    }

    @Override
    public void close() {
    }

    @Override
    public String getDisplayType() {
        return "Spawnly Global User Cap";
    }

    @Override
    public String getReferenceCategory() {
        return "spawnly-user-cap";
    }

    @Override
    public boolean isConfigurable() {
        return false;
    }

    @Override
    public Requirement[] getRequirementChoices() {
        return REQUIREMENT_CHOICES;
    }

    @Override
    public boolean isUserSetupAllowed() {
        return false;
    }

    @Override
    public String getHelpText() {
        return "Rejects self-serve registration once the realm user count reaches "
                + "the MAX_USERS ceiling. Unset/empty/non-positive MAX_USERS = "
                + "unlimited. Admin-API user creation bypasses it (it does not run "
                + "the registration flow).";
    }

    @Override
    public List<ProviderConfigProperty> getConfigProperties() {
        return Collections.emptyList();
    }
}
