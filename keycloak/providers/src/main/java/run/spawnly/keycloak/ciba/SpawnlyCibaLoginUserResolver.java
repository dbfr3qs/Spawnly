package run.spawnly.keycloak.ciba;

import org.keycloak.models.KeycloakSession;
import org.keycloak.models.UserModel;
import org.keycloak.models.utils.KeycloakModelUtils;
import org.keycloak.protocol.oidc.grants.ciba.resolvers.CIBALoginUserResolver;

/**
 * Resolves a CIBA request's {@code login_hint} to the Keycloak user who must
 * approve it. Ported from identityserver/CibaUserValidator.cs.
 *
 * <p>The platform threads the spawning user's id into the child sidecar, which
 * sends it as {@code login_hint="user:<id>"} (the same {@code user:<id>} identity
 * the platform's tokens carry). Keycloak's default resolver looks the hint up
 * verbatim, so a {@code user:alice} hint fails to match the username {@code
 * alice}. This resolver accepts BOTH the bare ({@code alice}) and prefixed
 * ({@code user:alice}) forms — exactly the C# {@code CibaUserValidator} which
 * strips a leading {@code user:} before {@code FindByUsername}.
 *
 * <p>{@code getInfoUsedByAuthentication} returns the resolved user's username
 * ({@code alice}); the {@link SpawnlyAuthenticationChannelProvider} then
 * cross-checks that username against the requesting agent's registry userId, so
 * the login_hint can never select an approver other than the agent's real user.
 */
public class SpawnlyCibaLoginUserResolver implements CIBALoginUserResolver {

    private static final String USER_PREFIX = "user:";

    private final KeycloakSession session;

    public SpawnlyCibaLoginUserResolver(KeycloakSession session) {
        this.session = session;
    }

    @Override
    public UserModel getUserFromLoginHint(String loginHint) {
        return findUser(loginHint);
    }

    @Override
    public String getInfoUsedByAuthentication(UserModel user) {
        // The username — the platform's human identity key (sub=user:<username>).
        return user.getUsername();
    }

    @Override
    public UserModel getUserFromInfoUsedByAuthentication(String info) {
        return findUser(info);
    }

    private UserModel findUser(String hint) {
        if (hint == null) {
            return null;
        }
        String username = hint.startsWith(USER_PREFIX) ? hint.substring(USER_PREFIX.length()) : hint;
        if (username.isEmpty()) {
            return null;
        }
        return KeycloakModelUtils.findUserByNameOrEmail(session, session.getContext().getRealm(), username);
    }

    @Override
    public void close() {
    }
}
