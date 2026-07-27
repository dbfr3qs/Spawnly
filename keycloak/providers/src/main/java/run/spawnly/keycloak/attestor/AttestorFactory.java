package run.spawnly.keycloak.attestor;

import org.jboss.logging.Logger;
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

    /**
     * Default expected SPIFFE trust domain (remote-substrate Phase 1), matching
     * the Go registry ({@code SPIRE_TRUST_DOMAIN}, default "cluster.local") and
     * the platform SPIRE deployment. A cluster deployed without the env behaves
     * exactly as today for platform (org-less) SVIDs, while org-scoped substrate
     * SVIDs get trust-domain-pinned + org-parsed. Enforcement is NOT disablable
     * via env: an empty {@code SPIRE_TRUST_DOMAIN} falls back to this default
     * (see {@link #resolve}); the fail-closed legacy passthrough is reachable
     * only via the bare {@link SpireCredentialVerifier} constructor (tests).
     */
    public static final String DEFAULT_SPIRE_TRUST_DOMAIN = "cluster.local";

    private static final Logger log = Logger.getLogger(AttestorFactory.class);

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
        String spireTrustDomain =
                resolve(config, "spireTrustDomain", "SPIRE_TRUST_DOMAIN", DEFAULT_SPIRE_TRUST_DOMAIN);

        switch (attestor) {
            case "":
            case "spiffe":
                // Trust-domain enforcement + org-scoped path parsing (Phase 1).
                return new SpireCredentialVerifier(
                        new SpireSvidValidator(spireJwksUrl), spireTrustDomain);
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
     * Builds a SEPARATE substrate-edge SPIFFE verifier for the SPIFFE-SVID routing
     * branch of {@link OrgAwareVerifier} (mint side, spec AC-5a) — the Keycloak
     * mirror of the Go registry's {@code REGISTRANT_SUBSTRATE_VERIFIER}. Returns
     * {@code null} UNLESS {@code ATTESTOR_SUBSTRATE_VERIFIER=spiffe}, so every
     * non-substrate deployment (including kind) is byte-identical.
     *
     * <p>WHY: on the AWS substrate edge the platform default is {@code aws-stsweb}
     * (correct for platform EKS agents), which REJECTS a JWT-SVID — so a substrate
     * agent minting with a SPIFFE JWT-SVID got {@code invalid_client}. Wiring this
     * verifier into {@code OrgAwareVerifier.withSpiffe} lets those SVIDs validate
     * while the stsweb path is untouched. Only the {@code aws-stsweb}/{@code aws-sts}
     * defaults ever need it: when the default is already {@code spiffe} (kind), the
     * non-STS branch already validates SVIDs, so this returns {@code null} — no
     * double build (bit-identical), mirroring the Go guard.
     *
     * <p>FAIL CLOSED: if enabled but {@code SPIRE_JWKS_URL} is missing/blank, or the
     * toggle carries an unknown value, this THROWS {@link IllegalStateException} at
     * init — the server never starts silently unable to validate substrate SVIDs,
     * and never returns a permissive verifier.
     *
     * <p>The verifier is constructed EXACTLY like the {@code spiffe} case of
     * {@link #build}: a trust-domain-enforcing {@link SpireCredentialVerifier} over
     * a {@link SpireSvidValidator} on {@code SPIRE_JWKS_URL}, pinned to
     * {@code SPIRE_TRUST_DOMAIN} (spawnly.run on AWS). The substrate sidecar mints
     * by presenting its SPIFFE JWT-SVID as {@code client_assertion}; that SVID is
     * fetched for audience = the Keycloak token-endpoint URL, and the Java SPIRE
     * validator does NOT assert audience (SpireSvidValidator, matching the C#), so
     * the mint SVID's audience is accepted with no sidecar change required.
     */
    public static AgentCredentialVerifier buildSubstrateSpiffe(Config.Scope config) {
        String toggle = resolve(config, "attestorSubstrateVerifier", "ATTESTOR_SUBSTRATE_VERIFIER", "");
        if (toggle.isEmpty()) {
            return null; // unset ⇒ no substrate branch ⇒ behaviour unchanged
        }
        if (!"spiffe".equals(toggle)) {
            // Unknown value ⇒ hard fail (never a silent misconfig), like the ATTESTOR
            // switch in build().
            throw new IllegalStateException("unknown ATTESTOR_SUBSTRATE_VERIFIER '" + toggle + "'");
        }
        String attestor = resolve(config, "attestor", "ATTESTOR", "spiffe");
        if (attestor.isEmpty() || "spiffe".equals(attestor) || "spire".equals(attestor)) {
            // The default verifier IS already the SPIRE verifier (kind): the non-STS
            // branch already validates SVIDs. Do NOT build a second one — keeps the
            // SPIRE-default deployment bit-identical (mirrors the Go guard).
            log.info("ATTESTOR_SUBSTRATE_VERIFIER=spiffe ignored: default ATTESTOR is already spiffe");
            return null;
        }
        // Read SPIRE_JWKS_URL WITHOUT the in-cluster default so a blank one FAILS
        // CLOSED (stricter than build(), which falls back to DEFAULT_SPIRE_JWKS_URL):
        // an operator who opted into the substrate branch but never wired a JWKS must
        // get a hard error, not a verifier that can never reach a discovery endpoint.
        String spireJwksUrl = resolve(config, "spireJwksUrl", "SPIRE_JWKS_URL", "");
        if (spireJwksUrl.isEmpty()) {
            throw new IllegalStateException("ATTESTOR_SUBSTRATE_VERIFIER=spiffe requires SPIRE_JWKS_URL");
        }
        String spireTrustDomain =
                resolve(config, "spireTrustDomain", "SPIRE_TRUST_DOMAIN", DEFAULT_SPIRE_TRUST_DOMAIN);
        log.infof("substrate-edge coexistence ENABLED (mint): non-STS SPIFFE-SVID mints validated by "
                + "spiffe (trust domain=%s jwks=%s)", spireTrustDomain, spireJwksUrl);
        return new SpireCredentialVerifier(new SpireSvidValidator(spireJwksUrl), spireTrustDomain);
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
