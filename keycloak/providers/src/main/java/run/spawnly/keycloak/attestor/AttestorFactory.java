package run.spawnly.keycloak.attestor;

import org.keycloak.Config;

/**
 * Selects the {@link AgentCredentialVerifier} implementation from the
 * {@code ATTESTOR} config, exactly like identityserver/Program.cs does:
 * {@code spiffe} (or empty) is the default; {@code aws-sts}/{@code aws-stsweb}
 * arrive in Phase 5; any unknown value is a HARD FAIL at startup (never a
 * silent fallback to an insecure default).
 *
 * <p>Config resolution mirrors the rest of the deployment: the value comes from
 * the {@code ATTESTOR} environment variable on the Keycloak Deployment (the
 * same env identity-server reads), with the Keycloak SPI config scope
 * ({@code --spi-...-attestor}) taking precedence if set. SPIRE JWKS URL comes
 * from {@code SPIRE_JWKS_URL} the same way.
 *
 * <p>This is the single seam extension point: Phase 5 adds the two AWS cases to
 * {@link #build}; nothing else in the authenticator changes.
 */
public final class AttestorFactory {

    public static final String DEFAULT_SPIRE_JWKS_URL =
            "http://spire-spiffe-oidc-discovery-provider.spire-system/.well-known/jwks.json";

    private AttestorFactory() {
    }

    /**
     * Builds the configured verifier or throws {@link IllegalStateException}
     * for an unknown attestor (hard fail, matching Program.cs). {@code config}
     * may be null (falls back entirely to env).
     */
    public static AgentCredentialVerifier build(Config.Scope config) {
        String attestor = resolve(config, "attestor", "ATTESTOR", "spiffe");
        String spireJwksUrl = resolve(config, "spireJwksUrl", "SPIRE_JWKS_URL", DEFAULT_SPIRE_JWKS_URL);

        switch (attestor) {
            case "":
            case "spiffe":
                return new SpireCredentialVerifier(new SpireSvidValidator(spireJwksUrl));
            // Phase 5 extension points — the AWS verifiers plug in here behind
            // the same seam, no authenticator change:
            //   case "aws-sts":    return new StsCredentialVerifier(...);
            //   case "aws-stsweb": return new StsWebCredentialVerifier(...);
            default:
                throw new IllegalStateException("unknown ATTESTOR '" + attestor + "'");
        }
    }

    /**
     * Resolves a setting from (in order) the SPI config scope, then the
     * environment variable, then the default.
     */
    static String resolve(Config.Scope config, String spiKey, String envKey, String def) {
        if (config != null) {
            String v = config.get(spiKey);
            if (v != null && !v.isEmpty()) {
                return v;
            }
        }
        String env = System.getenv(envKey);
        if (env != null && !env.isEmpty()) {
            return env;
        }
        return def;
    }
}
