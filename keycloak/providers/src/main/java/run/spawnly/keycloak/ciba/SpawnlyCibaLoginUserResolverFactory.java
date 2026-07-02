package run.spawnly.keycloak.ciba;

import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.protocol.oidc.grants.ciba.resolvers.CIBALoginUserResolver;
import org.keycloak.protocol.oidc.grants.ciba.resolvers.CIBALoginUserResolverFactory;

/**
 * Factory for {@link SpawnlyCibaLoginUserResolver}, registered via
 * META-INF/services/…CIBALoginUserResolverFactory. Selected as the realm's CIBA
 * login-user resolver at build time
 * (Dockerfile: {@code --spi-ciba-auth-user-resolver-provider-default=spawnly-ciba-user-resolver}),
 * replacing the default resolver so a {@code login_hint="user:<id>"} resolves to
 * the {@code <id>} user (parity with the C# CibaUserValidator).
 */
public class SpawnlyCibaLoginUserResolverFactory implements CIBALoginUserResolverFactory {

    public static final String PROVIDER_ID = "spawnly-ciba-user-resolver";

    @Override
    public CIBALoginUserResolver create(KeycloakSession session) {
        return new SpawnlyCibaLoginUserResolver(session);
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
    public String getId() {
        return PROVIDER_ID;
    }
}
