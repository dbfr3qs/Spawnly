package run.spawnly.keycloak.attestor;

import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.MACSigner;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jose.jwk.gen.RSAKeyGenerator;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.PlainJWT;
import com.nimbusds.jwt.SignedJWT;

import java.util.Date;

/**
 * Test-only helper: mints signed JWT-SVIDs and the matching JWKS document, so
 * the SPIRE validator can be exercised end-to-end against a WireMock JWKS.
 * Uses Nimbus (test scope) purely to SIGN; production code never touches it.
 */
final class SvidTestFactory {

    final RSAKey key;   // full key (private) for signing
    final String kid;

    SvidTestFactory() throws Exception {
        this.kid = "test-spire-key-1";
        this.key = new RSAKeyGenerator(2048).keyID(kid).generate();
    }

    /** A second, unrelated signer — for the "signed by the wrong key" case. */
    static SvidTestFactory foreign() throws Exception {
        SvidTestFactory f = new SvidTestFactory("foreign-key");
        return f;
    }

    private SvidTestFactory(String kid) throws Exception {
        this.kid = kid;
        this.key = new RSAKeyGenerator(2048).keyID(kid).generate();
    }

    /** The JWKS document (public keys only) that WireMock serves. */
    String jwksJson() {
        return "{\"keys\":[" + key.toPublicJWK().toJSONString() + "]}";
    }

    /** A valid SVID: subject = spiffeId, aud = audience, exp = now + 5min. */
    String svid(String spiffeId, String audience) throws Exception {
        return svid(spiffeId, audience, new Date(System.currentTimeMillis() + 300_000));
    }

    /** An SVID with an explicit expiry (use a past Date for the expired case). */
    String svid(String spiffeId, String audience, Date exp) throws Exception {
        SignedJWT jwt = new SignedJWT(
                new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(kid).type(null).build(),
                claims(spiffeId, audience, exp));
        jwt.sign(new RSASSASigner(key));
        return jwt.serialize();
    }

    /**
     * An UNSECURED token (alg=none, empty signature) with a valid-looking
     * subject — the classic "none algorithm" bypass. Must be rejected.
     */
    String algNoneSvid(String spiffeId) {
        return new PlainJWT(claims(spiffeId, "keys",
                new Date(System.currentTimeMillis() + 300_000))).serialize();
    }

    /**
     * An HS256 token signed with the RSA PUBLIC key's bytes as the HMAC secret —
     * the classic JWT algorithm-confusion attack (an attacker who reads the
     * public JWKS can forge this). Must be rejected: the JWKS key is RSA, so a
     * validator that honours the key type will never treat it as an HMAC secret.
     */
    String hs256ConfusionSvid(String spiffeId) throws Exception {
        byte[] publicKeyBytes = key.toPublicKey().getEncoded(); // public knowledge
        SignedJWT jwt = new SignedJWT(
                new JWSHeader.Builder(JWSAlgorithm.HS256).keyID(kid).build(),
                claims(spiffeId, "keys", new Date(System.currentTimeMillis() + 300_000)));
        jwt.sign(new MACSigner(publicKeyBytes));
        return jwt.serialize();
    }

    private static JWTClaimsSet claims(String spiffeId, String audience, Date exp) {
        return new JWTClaimsSet.Builder()
                .subject(spiffeId)
                .audience(audience)
                .issuer("https://spire")
                .issueTime(new Date(System.currentTimeMillis() - 60_000))
                .expirationTime(exp)
                .build();
    }
}
