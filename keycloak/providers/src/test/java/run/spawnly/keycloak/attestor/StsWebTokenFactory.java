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
import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Test-only helper: mints signed {@code sts:GetWebIdentityToken}-shaped JWTs and
 * the matching JWKS, so {@link StsWebCredentialVerifier} can be exercised
 * end-to-end against a WireMock JWKS. Uses Nimbus (test scope) purely to SIGN;
 * production code validates with Keycloak's own crypto and never touches Nimbus.
 */
final class StsWebTokenFactory {

    static final String NAMESPACE_CLAIM = "https://sts.amazonaws.com/";

    final RSAKey key;
    final String kid;

    StsWebTokenFactory() throws Exception {
        this.kid = "test-sts-key-1";
        this.key = new RSAKeyGenerator(2048).keyID(kid).generate();
    }

    private StsWebTokenFactory(String kid) throws Exception {
        this.kid = kid;
        this.key = new RSAKeyGenerator(2048).keyID(kid).generate();
    }

    /** A second, unrelated signer — for the "signed by the wrong key" case. */
    static StsWebTokenFactory foreign() throws Exception {
        return new StsWebTokenFactory("foreign-sts-key");
    }

    String jwksJson() {
        return "{\"keys\":[" + key.toPublicJWK().toJSONString() + "]}";
    }

    /** A set of EKS-attested principal_tags. */
    static Map<String, Object> tags(String namespace, String serviceAccount,
                                     String clusterArn, String podName) {
        Map<String, Object> t = new LinkedHashMap<>();
        if (namespace != null) t.put("kubernetes-namespace", namespace);
        if (serviceAccount != null) t.put("kubernetes-service-account", serviceAccount);
        if (clusterArn != null) t.put("eks-cluster-arn", clusterArn);
        if (podName != null) t.put("kubernetes-pod-name", podName);
        return t;
    }

    /** A valid, RS256-signed web-identity token (exp = now + 5min). */
    String token(String issuer, String audience, Map<String, Object> principalTags) throws Exception {
        return token(issuer, audience, principalTags,
                new Date(System.currentTimeMillis() + 300_000));
    }

    String token(String issuer, String audience, Map<String, Object> principalTags, Date exp)
            throws Exception {
        SignedJWT jwt = new SignedJWT(
                new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(kid).build(),
                claims(issuer, audience, principalTags, exp));
        jwt.sign(new RSASSASigner(key));
        return jwt.serialize();
    }

    /** A token whose namespaced claim has no principal_tags object at all. */
    String tokenWithoutPrincipalTags(String issuer, String audience) throws Exception {
        Map<String, Object> ns = new LinkedHashMap<>();
        ns.put("something-else", "x");
        JWTClaimsSet.Builder b = new JWTClaimsSet.Builder()
                .subject("arn:aws:sts::123:assumed-role/r/pod")
                .issuer(issuer)
                .audience(audience)
                .issueTime(new Date(System.currentTimeMillis() - 60_000))
                .expirationTime(new Date(System.currentTimeMillis() + 300_000))
                .claim(NAMESPACE_CLAIM, ns);
        SignedJWT jwt = new SignedJWT(
                new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(kid).build(), b.build());
        jwt.sign(new RSASSASigner(key));
        return jwt.serialize();
    }

    /** alg=none unsecured token — the classic bypass; must be rejected. */
    String algNoneToken(String issuer, String audience, Map<String, Object> principalTags) {
        return new PlainJWT(claims(issuer, audience, principalTags,
                new Date(System.currentTimeMillis() + 300_000))).serialize();
    }

    /**
     * HS256 token signed with the RSA PUBLIC key bytes as the HMAC secret — the
     * classic algorithm-confusion attack. Must be rejected: the JWKS key is RSA,
     * so a validator honouring the key type never treats it as an HMAC secret.
     */
    String hs256ConfusionToken(String issuer, String audience, Map<String, Object> principalTags)
            throws Exception {
        byte[] publicKeyBytes = key.toPublicKey().getEncoded(); // public knowledge
        SignedJWT jwt = new SignedJWT(
                new JWSHeader.Builder(JWSAlgorithm.HS256).keyID(kid).build(),
                claims(issuer, audience, principalTags,
                        new Date(System.currentTimeMillis() + 300_000)));
        jwt.sign(new MACSigner(publicKeyBytes));
        return jwt.serialize();
    }

    private static JWTClaimsSet claims(String issuer, String audience,
                                       Map<String, Object> principalTags, Date exp) {
        Map<String, Object> ns = new LinkedHashMap<>();
        ns.put("principal_tags", principalTags);
        return new JWTClaimsSet.Builder()
                .subject("arn:aws:sts::123456789012:assumed-role/eks-role/pod")
                .issuer(issuer)
                .audience(audience)
                .issueTime(new Date(System.currentTimeMillis() - 60_000))
                .expirationTime(exp)
                .claim(NAMESPACE_CLAIM, ns)
                .build();
    }
}
