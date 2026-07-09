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
            // AWS attestors plug in behind the same seam — no authenticator
            // change. Mirrors identityserver/Program.cs's ATTESTOR switch.
            case "aws-sts":
                // STS_TRUSTED_ENDPOINT (empty by default) NARROWS the trusted STS
                // endpoint so an in-cluster fake STS can stand in for e2e; unset in
                // production, so the AWS host pattern applies (see the field's SSRF
                // note). Same override the Go replay verifier honours.
                return new StsCredentialVerifier(
                        resolve(config, "stsTrustedEndpoint", StsCredentialVerifier.STS_TRUSTED_ENDPOINT_ENV, ""));
            case "aws-stsweb":
                return new StsWebCredentialVerifier(stsWebOptions(config));
            default:
                throw new IllegalStateException("unknown ATTESTOR '" + attestor + "'");
        }
    }

    /**
     * Resolves the {@code STS_TRUSTED_ENDPOINT} override (Phase 8) the same way
     * {@link #build} does for the {@code aws-sts} attestor. Empty by default (the
     * production stance: the hardcoded AWS STS host pattern applies); set to an
     * in-cluster fake STS for e2e. The Phase-9 {@link OrgAwareVerifier} builds its
     * org-bound STS replay verifier with this so it honours the SAME endpoint
     * narrowing regardless of which attestor is the platform default.
     */
    public static String resolveStsTrustedEndpoint(Config.Scope config) {
        return resolve(config, "stsTrustedEndpoint", StsCredentialVerifier.STS_TRUSTED_ENDPOINT_ENV, "");
    }

    /**
     * Reads the {@code STSWEB_*} options the way identityserver/Program.cs:42
     * does: audience defaults to "spawnly"; when {@code STSWEB_JWKS_URL} is
     * empty but {@code STSWEB_ISSUER} is set, the JWKS URL is derived as
     * {@code issuer + "/.well-known/jwks.json"}. namespace / service-account /
     * cluster-ARN default to empty (not enforced), matching today's posture.
     */
    static StsWebOptions stsWebOptions(Config.Scope config) {
        String issuer = resolve(config, "stswebIssuer", "STSWEB_ISSUER", "");
        String jwksUrl = resolve(config, "stswebJwksUrl", "STSWEB_JWKS_URL", "");
        if (jwksUrl.isEmpty() && !issuer.isEmpty()) {
            jwksUrl = trimTrailingSlash(issuer) + "/.well-known/jwks.json";
        }
        return new StsWebOptions(
                jwksUrl,
                issuer,
                resolve(config, "stswebAudience", "STSWEB_AUDIENCE", "spawnly"),
                resolve(config, "stswebNamespace", "STSWEB_NAMESPACE", ""),
                resolve(config, "stswebServiceAccount", "STSWEB_SERVICE_ACCOUNT", ""),
                resolve(config, "stswebClusterArn", "STSWEB_CLUSTER_ARN", ""),
                "");
    }

    private static String trimTrailingSlash(String s) {
        int end = s.length();
        while (end > 0 && s.charAt(end - 1) == '/') {
            end--;
        }
        return s.substring(0, end);
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
