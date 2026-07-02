package run.spawnly.keycloak.attestor;

import org.jboss.logging.Logger;
import org.keycloak.TokenVerifier;
import org.keycloak.jose.jwk.JSONWebKeySet;
import org.keycloak.jose.jwk.JWK;
import org.keycloak.jose.jwk.JWKParser;
import org.keycloak.jose.jws.JWSInput;
import org.keycloak.representations.JsonWebToken;
import org.keycloak.util.JsonSerialization;

import javax.net.ssl.SSLContext;
import javax.net.ssl.SSLParameters;
import javax.net.ssl.TrustManager;
import javax.net.ssl.X509TrustManager;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.security.PublicKey;
import java.security.cert.X509Certificate;
import java.time.Duration;

/**
 * Validates SPIRE JWT-SVIDs against the SPIRE OIDC discovery provider JWKS.
 * Ported from identityserver/SpireSvidValidator.cs.
 *
 * <p>Implemented on KEYCLOAK'S OWN crypto ({@link TokenVerifier},
 * {@link JWKParser}) rather than a third-party JOSE library, so this stays a
 * thin providers-only JAR with no shaded dependencies — Keycloak does NOT
 * bundle Nimbus, so pulling it in would need shading.
 *
 * <p>The SPIRE OIDC discovery provider serves its JWKS over HTTPS with a
 * SELF-SIGNED certificate, so TLS verification is relaxed here EXACTLY as the
 * C# does ({@code ServerCertificateCustomValidationCallback = () => true}).
 * This is safe: the endpoint is an in-cluster ClusterIP reachable only over
 * in-cluster hops, and the JWT-SVID signature itself establishes trust.
 *
 * <p>Validation checks signature + expiry (via {@code TokenVerifier.IS_ACTIVE}).
 * Issuer/type are NOT checked (SVIDs carry a SPIFFE issuer + {@code typ=JWT},
 * and the C# sets {@code ValidateIssuer=false}); AUDIENCE is checked by the
 * caller layer where a fixed audience exists (the C# also disables audience
 * validation here — SPIRE issues the SVID for the "keys"/registry audience the
 * sidecar requests, and the token-endpoint client-auth path has no single fixed
 * audience to assert).
 */
public class SpireSvidValidator {

    private static final Logger log = Logger.getLogger(SpireSvidValidator.class);

    private final String spireJwksUrl;
    private final HttpClient http;

    public SpireSvidValidator(String spireJwksUrl) {
        this(spireJwksUrl, insecureHttpClient());
    }

    /** For tests: inject a client (e.g. pointed at WireMock over plain http). */
    SpireSvidValidator(String spireJwksUrl, HttpClient http) {
        this.spireJwksUrl = spireJwksUrl;
        this.http = http;
    }

    /**
     * Validates the signature + expiry of a SPIRE JWT-SVID against the SPIRE
     * JWKS. Returns the SPIFFE URI (the SVID subject) on success, or
     * {@code null} on any failure. Never throws — a null return is the single
     * "invalid" signal, mirroring the C# catch-all.
     */
    public String validateAndGetSpiffeId(String svid) {
        if (svid == null || svid.isEmpty()) {
            return null;
        }
        try {
            JWK[] keys = fetchJwks();
            if (keys == null || keys.length == 0) {
                return null;
            }

            TokenVerifier<JsonWebToken> verifier = TokenVerifier.create(svid, JsonWebToken.class)
                    // JWT-SVIDs carry typ=JWT, not the KC-internal token type; a
                    // SPIFFE issuer, not the realm — so skip those checks. Keep
                    // IS_ACTIVE (exp/nbf/iat) which is the security-relevant one.
                    .checkTokenType(false)
                    .checkRealmUrl(false)
                    .withChecks(TokenVerifier.IS_ACTIVE);

            PublicKey key = selectKey(svid, keys);
            if (key == null) {
                return null;
            }
            verifier.publicKey(key);
            verifier.verify(); // throws on bad signature / expired token

            return verifier.getToken().getSubject();
        } catch (Exception e) {
            // parse / network / bad JWKS / bad signature / expired ⇒ rejected.
            log.debugf(e, "SPIRE JWT-SVID validation failed");
            return null;
        }
    }

    private JWK[] fetchJwks() throws Exception {
        HttpRequest req = HttpRequest.newBuilder(URI.create(spireJwksUrl))
                .timeout(Duration.ofSeconds(10))
                .GET()
                .build();
        HttpResponse<String> resp = http.send(req, HttpResponse.BodyHandlers.ofString());
        if (resp.statusCode() != 200) {
            log.debugf("SPIRE JWKS fetch returned status %d", resp.statusCode());
            return null;
        }
        JSONWebKeySet jwks = JsonSerialization.readValue(resp.body(), JSONWebKeySet.class);
        return jwks.getKeys();
    }

    /**
     * Picks the JWKS key matching the SVID's {@code kid} header (falling back to
     * the sole key if there is exactly one and no kid match), and returns its
     * PublicKey. Matching on kid prevents a valid signature from an unrelated
     * key in a multi-key set from being accepted.
     */
    private PublicKey selectKey(String svid, JWK[] keys) throws Exception {
        JWSInput input = new JWSInput(svid);
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

    /**
     * An HttpClient that trusts any TLS certificate — required because the
     * SPIRE OIDC discovery provider presents a self-signed cert. See the class
     * javadoc for why this is safe here (in-cluster only + JWS trust).
     */
    private static HttpClient insecureHttpClient() {
        try {
            SSLContext ctx = SSLContext.getInstance("TLS");
            ctx.init(null, new TrustManager[]{new TrustAllManager()}, new java.security.SecureRandom());
            // Disable hostname verification for THIS client only (the SPIRE
            // cert's SAN may not match the service DNS name). Scoped via
            // SSLParameters — an empty endpoint-identification algorithm turns
            // the hostname check off — rather than the process-wide
            // `jdk.internal.httpclient.disableHostnameVerification` system
            // property, which would weaken TLS for EVERY other HttpClient in the
            // Keycloak JVM. Trust-all + no-hostname is safe only here: the SPIRE
            // OIDC endpoint is an in-cluster ClusterIP and the JWT-SVID signature
            // is the real trust anchor.
            SSLParameters sslParams = new SSLParameters();
            sslParams.setEndpointIdentificationAlgorithm("");
            return HttpClient.newBuilder()
                    .sslContext(ctx)
                    .sslParameters(sslParams)
                    .connectTimeout(Duration.ofSeconds(10))
                    .build();
        } catch (Exception e) {
            throw new IllegalStateException("failed to build SPIRE HTTP client", e);
        }
    }

    private static final class TrustAllManager implements X509TrustManager {
        @Override public void checkClientTrusted(X509Certificate[] chain, String authType) {}
        @Override public void checkServerTrusted(X509Certificate[] chain, String authType) {}
        @Override public X509Certificate[] getAcceptedIssuers() { return new X509Certificate[0]; }
    }
}
