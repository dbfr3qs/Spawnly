package run.spawnly.keycloak.exchange;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jose.jwk.gen.RSAKeyGenerator;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;

import java.util.Date;
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Test-only helper: mints realm-issued-style subject_tokens (RS256, matching
 * Keycloak's access-token shape — {@code iss}, {@code sub}, single-string
 * {@code scope}, nested {@code act} object) and exposes the public key so the
 * test can wire {@code session.keys().getKey(...)} to return it. Uses Nimbus
 * (test scope only) to sign; production never touches it.
 */
final class ExchangeTestFactory {

    static final String ISSUER = "http://keycloak:8080/realms/spawnly";

    final RSAKey key;
    final String kid;

    ExchangeTestFactory() throws Exception {
        this.kid = "test-realm-sig-1";
        this.key = new RSAKeyGenerator(2048).keyID(kid).generate();
    }

    /** A distinct signer with a different kid — the "signed by a foreign key" case. */
    static ExchangeTestFactory foreign() throws Exception {
        ExchangeTestFactory f = new ExchangeTestFactory("foreign-realm-key");
        return f;
    }

    private ExchangeTestFactory(String kid) throws Exception {
        this.kid = kid;
        this.key = new RSAKeyGenerator(2048).keyID(kid).generate();
    }

    /**
     * A realm-issued subject_token. {@code act} is the nested actor object
     * (pass null for a first-hop token with no prior chain).
     */
    String subjectToken(String sub, String scope, Map<String, Object> act) throws Exception {
        return subjectToken(sub, scope, act, ISSUER, new Date(System.currentTimeMillis() + 300_000));
    }

    /** Variant with an explicit issuer (for the foreign-issuer test) and expiry. */
    String subjectToken(String sub, String scope, Map<String, Object> act, String issuer, Date exp)
            throws Exception {
        JWTClaimsSet.Builder claims = new JWTClaimsSet.Builder()
                .subject(sub)
                .issuer(issuer)
                .claim("scope", scope)
                .issueTime(new Date(System.currentTimeMillis() - 60_000))
                .expirationTime(exp);
        if (act != null) {
            claims.claim("act", act);
        }
        SignedJWT jwt = new SignedJWT(
                new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(kid).build(),
                claims.build());
        jwt.sign(new RSASSASigner(key));
        return jwt.serialize();
    }

    /** Nested act object {"sub": spiffe} (optionally wrapping a prior act). */
    static Map<String, Object> act(String spiffe, Map<String, Object> inner) {
        Map<String, Object> m = new LinkedHashMap<>();
        m.put("sub", spiffe);
        if (inner != null) {
            m.put("act", inner);
        }
        return m;
    }
}
