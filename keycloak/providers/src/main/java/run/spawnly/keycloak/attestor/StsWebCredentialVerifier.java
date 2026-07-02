package run.spawnly.keycloak.attestor;

import org.jboss.logging.Logger;
import org.keycloak.TokenVerifier;
import org.keycloak.jose.jwk.JSONWebKeySet;
import org.keycloak.jose.jwk.JWK;
import org.keycloak.jose.jwk.JWKParser;
import org.keycloak.jose.jws.JWSInput;
import org.keycloak.representations.JsonWebToken;
import org.keycloak.util.JsonSerialization;

import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.security.PublicKey;
import java.time.Duration;
import java.time.Instant;
import java.util.Map;
import java.util.concurrent.locks.ReentrantLock;

/**
 * AWS STS web-identity attestor ({@code aws-stsweb}, outbound web-identity
 * federation). Validates a {@code sts:GetWebIdentityToken} JWT against the
 * account STS issuer's JWKS and derives the agent id from the EKS-set,
 * cluster-attested {@code principal_tags.kubernetes-pod-name} — NOT from
 * caller-supplied {@code request_tags}.
 *
 * <p>Ported from identityserver/StsWebCredentialVerifier.cs and kept in lock
 * step with Go {@code internal/registrant/stsweb.go} ({@code identityFromTags})
 * so the registry and the IdP derive the SAME identity (the consistency
 * invariant): {@code agentId = <kubernetes-pod-name minus podSuffix>},
 * {@code subject = "<eks-cluster-arn or 'eks'>/agent/<agentId>"},
 * {@code issuer = "aws-stsweb"}. The path-style subject lets the token-exchange
 * act-chain handling ({@code path.Base}) recover the agentId, matching SPIRE's
 * shape.
 *
 * <p>Signature + lifetime validation runs on KEYCLOAK'S OWN crypto
 * ({@link TokenVerifier}, {@link JWKParser}), like {@link SpireSvidValidator},
 * so this stays a thin providers-only JAR — no third-party JOSE library and no
 * AWS SDK are pulled onto the runtime classpath.
 *
 * <p>Unlike the SPIRE validator, the STS JWKS is PUBLIC and TLS-valid, so this
 * uses a normal (verifying) {@link HttpClient} — no relaxed TLS. The JWKS
 * rarely rotates and tokens live ≤1h, so an hour-long cache keeps the JWKS
 * endpoint off the per-mint hot path; a signature failure on cached keys
 * triggers a single forced refresh to ride out a mid-cache rotation (mirroring
 * the C# behaviour).
 */
public class StsWebCredentialVerifier implements AgentCredentialVerifier {

    private static final Logger log = Logger.getLogger(StsWebCredentialVerifier.class);

    // The namespaced claim STS puts identity context under in a
    // GetWebIdentityToken JWT.
    private static final String STS_NAMESPACE_CLAIM = "https://sts.amazonaws.com/";
    private static final Duration CACHE_TTL = Duration.ofHours(1);

    private final StsWebOptions o;
    private final String podSuffix;
    private final HttpClient http;

    private final ReentrantLock refreshLock = new ReentrantLock();
    private volatile JWK[] cachedKeys;
    private volatile Instant cacheExpiry = Instant.EPOCH;

    public StsWebCredentialVerifier(StsWebOptions options) {
        this(options, HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(10)).build());
    }

    /** For tests: inject a client (e.g. pointed at WireMock over plain http). */
    StsWebCredentialVerifier(StsWebOptions options, HttpClient http) {
        this.o = options;
        this.http = http;
        this.podSuffix = (options.podSuffix() == null || options.podSuffix().isEmpty())
                ? "-pod" : options.podSuffix();
        if (options.clusterArn() == null || options.clusterArn().isEmpty()) {
            log.warn("STSWEB_CLUSTER_ARN unset — accepting aws-stsweb tokens from any EKS cluster in the account");
        }
    }

    @Override
    public AgentIdentity verify(String credential) {
        if (credential == null || credential.isEmpty()) {
            return null;
        }
        try {
            // First attempt on cached keys; on a signature/verification failure
            // with cached keys, force one refresh and retry (rides a rotation).
            JsonWebToken token = validate(credential, false);
            if (token == null && cameFromCache()) {
                token = validate(credential, true);
            }
            if (token == null) {
                return null;
            }
            return identityFromToken(token);
        } catch (Exception e) {
            log.debugf(e, "aws-stsweb verification failed");
            return null;
        }
    }

    private boolean fromCacheLastRead;

    private boolean cameFromCache() {
        return fromCacheLastRead;
    }

    /**
     * Validates signature, lifetime, issuer, and audience. Returns the token on
     * success or null on any failure (never throws for a bad token). Issuer /
     * audience are enforced only when configured, exactly like the C#
     * {@code ValidateIssuer}/{@code ValidateAudience} gated on non-empty.
     */
    private JsonWebToken validate(String jwtStr, boolean forceRefresh) {
        try {
            JWK[] keys = signingKeys(forceRefresh);
            if (keys == null || keys.length == 0) {
                return null;
            }
            PublicKey key = selectKey(jwtStr, keys);
            if (key == null) {
                return null;
            }

            TokenVerifier<JsonWebToken> verifier = TokenVerifier.create(jwtStr, JsonWebToken.class)
                    .checkTokenType(false)
                    .checkRealmUrl(false)
                    .withChecks(TokenVerifier.IS_ACTIVE) // exp/nbf/iat
                    .publicKey(key);

            String issuer = o.issuer();
            if (issuer != null && !issuer.isEmpty()) {
                verifier.withChecks(new TokenVerifier.RealmUrlCheck(issuer));
            }
            String audience = o.audience();
            if (audience != null && !audience.isEmpty()) {
                verifier.audience(audience);
            }

            verifier.verify(); // throws on bad signature / expired / wrong iss / wrong aud
            return verifier.getToken();
        } catch (Exception e) {
            log.debugf(e, "aws-stsweb token failed validation");
            return null;
        }
    }

    private AgentIdentity identityFromToken(JsonWebToken token) {
        Map<String, Object> other = token.getOtherClaims();
        Object nsClaim = other == null ? null : other.get(STS_NAMESPACE_CLAIM);
        if (!(nsClaim instanceof Map)) {
            log.debugf("aws-stsweb token missing %s object", STS_NAMESPACE_CLAIM);
            return null;
        }
        Object ptClaim = ((Map<?, ?>) nsClaim).get("principal_tags");
        if (!(ptClaim instanceof Map)) {
            log.debug("aws-stsweb token missing principal_tags (pod not under EKS Pod Identity?)");
            return null;
        }
        Map<?, ?> tags = (Map<?, ?>) ptClaim;

        // Defense in depth: assert the EKS-attested context matches expectations.
        if (!matches(tag(tags, "kubernetes-namespace"), o.namespace())
                || !matches(tag(tags, "kubernetes-service-account"), o.serviceAccount())
                || !matches(tag(tags, "eks-cluster-arn"), o.clusterArn())) {
            log.debug("aws-stsweb attested context did not match expected namespace/service-account/cluster");
            return null;
        }

        String podName = tag(tags, "kubernetes-pod-name");
        if (podName == null || podName.isEmpty()) {
            log.debug("aws-stsweb token missing principal_tags.kubernetes-pod-name");
            return null;
        }

        String agentId = podName.endsWith(podSuffix)
                ? podName.substring(0, podName.length() - podSuffix.length())
                : podName;
        if (agentId.isEmpty()) {
            log.debugf("aws-stsweb pod name %s yields empty agentId", podName);
            return null;
        }

        // Empty-or-missing eks-cluster-arn falls back to "eks" — kept in lock
        // step with Go registrant.identityFromTags (the consistency invariant).
        String arn = tag(tags, "eks-cluster-arn");
        String clusterArn = (arn == null || arn.isEmpty()) ? "eks" : arn;
        return new AgentIdentity(agentId, clusterArn + "/agent/" + agentId, "aws-stsweb");
    }

    private static String tag(Map<?, ?> tags, String key) {
        Object v = tags.get(key);
        return (v instanceof String) ? (String) v : null;
    }

    private static boolean matches(String got, String want) {
        return want == null || want.isEmpty() || want.equals(got);
    }

    /**
     * Returns the STS issuer's signing keys from an hour-long cache; records
     * whether they came from cache (via {@link #fromCacheLastRead}) so the
     * caller can decide whether a validation failure warrants a forced refresh.
     */
    private JWK[] signingKeys(boolean forceRefresh) throws Exception {
        if (!forceRefresh && cachedKeys != null && Instant.now().isBefore(cacheExpiry)) {
            fromCacheLastRead = true;
            return cachedKeys;
        }
        refreshLock.lock();
        try {
            if (!forceRefresh && cachedKeys != null && Instant.now().isBefore(cacheExpiry)) {
                fromCacheLastRead = true;
                return cachedKeys;
            }
            JWK[] fetched = fetchJwks();
            cachedKeys = fetched;
            cacheExpiry = Instant.now().plus(CACHE_TTL);
            fromCacheLastRead = false;
            return fetched;
        } finally {
            refreshLock.unlock();
        }
    }

    private JWK[] fetchJwks() throws Exception {
        HttpRequest req = HttpRequest.newBuilder(URI.create(o.jwksUrl()))
                .timeout(Duration.ofSeconds(10))
                .GET()
                .build();
        HttpResponse<String> resp = http.send(req, HttpResponse.BodyHandlers.ofString());
        if (resp.statusCode() != 200) {
            log.debugf("aws-stsweb JWKS fetch returned status %d", resp.statusCode());
            return null;
        }
        JSONWebKeySet jwks = JsonSerialization.readValue(resp.body(), JSONWebKeySet.class);
        return jwks.getKeys();
    }

    /**
     * Picks the JWKS key matching the token's {@code kid} header (falling back
     * to the sole key when there is exactly one and no kid match) and returns
     * its PublicKey. Deriving the key type from the JWKS (RSA) — not from the
     * JWS header {@code alg} — is what makes this alg-confusion-safe: an HS256
     * "signature" is never treated as valid against an RSA public key.
     */
    private static PublicKey selectKey(String jwt, JWK[] keys) throws Exception {
        JWSInput input = new JWSInput(jwt);
        String kid = input.getHeader().getKeyId();

        JWK match = null;
        for (JWK k : keys) {
            if (kid != null && kid.equals(k.getKeyId())) {
                match = k;
                break;
            }
        }
        if (match == null && kid == null && keys.length == 1) {
            match = keys[0];
        }
        if (match == null) {
            return null;
        }
        return JWKParser.create(match).toPublicKey();
    }
}
