package run.spawnly.keycloak.attestor;

import com.github.tomakehurst.wiremock.WireMockServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.net.http.HttpClient;
import java.util.Date;
import java.util.Map;

import static com.github.tomakehurst.wiremock.client.WireMock.aResponse;
import static com.github.tomakehurst.wiremock.client.WireMock.get;
import static com.github.tomakehurst.wiremock.client.WireMock.urlEqualTo;
import static com.github.tomakehurst.wiremock.core.WireMockConfiguration.options;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;

/**
 * Ports the identityserver StsWebCredentialVerifier behaviour ({@code aws-stsweb})
 * and verifies parity with the Go registrant verifier
 * ({@code internal/registrant/stsweb.go}, {@code identityFromTags}):
 * agentId = kubernetes-pod-name minus the "-pod" suffix, subject =
 * "&lt;eks-cluster-arn or 'eks'&gt;/agent/&lt;agentId&gt;", issuer = "aws-stsweb".
 * Signature/issuer/audience are validated; the EKS-attested namespace /
 * service-account / cluster-ARN are enforced; alg=none and HS256 confusion are
 * rejected.
 */
class StsWebCredentialVerifierTest {

    // Shared vectors with internal/registrant/stsweb_test.go.
    private static final String ISSUER = "https://oidc.eks.example/id/ABCD";
    private static final String AUDIENCE = "spawnly";
    private static final String NS = "default";
    private static final String SA = "spawnly-agent";
    private static final String CLUSTER = "arn:aws:eks:us-east-1:123456789012:cluster/spawnly";
    private static final String POD = "agent-7e5fc77f-pod";
    private static final String AGENT_ID = "agent-7e5fc77f";

    private WireMockServer wm;
    private StsWebTokenFactory f;
    private String jwksUrl;

    @BeforeEach
    void setUp() throws Exception {
        wm = new WireMockServer(options().dynamicPort());
        wm.start();
        f = new StsWebTokenFactory();
        wm.stubFor(get(urlEqualTo("/jwks")).willReturn(aResponse()
                .withHeader("Content-Type", "application/json").withBody(f.jwksJson())));
        jwksUrl = "http://localhost:" + wm.port() + "/jwks";
    }

    @AfterEach
    void tearDown() {
        if (wm != null) wm.stop();
    }

    private StsWebCredentialVerifier verifier(StsWebOptions o) {
        return new StsWebCredentialVerifier(o, HttpClient.newHttpClient());
    }

    private StsWebOptions opts() {
        return new StsWebOptions(jwksUrl, ISSUER, AUDIENCE, NS, SA, CLUSTER, "");
    }

    @Test
    void validToken_returnsIdentity_agentIdFromPodName() throws Exception {
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD));

        AgentIdentity id = verifier(opts()).verify(jwt);

        assertNotNull(id);
        assertEquals(AGENT_ID, id.agentId());
        assertEquals(CLUSTER + "/agent/" + AGENT_ID, id.subject());
        assertEquals("aws-stsweb", id.issuer());
    }

    @Test
    void missingClusterArnConfig_fallsBackToEksSubjectPrefix() throws Exception {
        // No eks-cluster-arn tag AND no configured expectation ⇒ subject prefix
        // falls back to "eks" (parity with Go identityFromTags). Cluster check
        // disabled (want empty), so any cluster is accepted.
        StsWebOptions o = new StsWebOptions(jwksUrl, ISSUER, AUDIENCE, NS, SA, "", "");
        String jwt = f.token(ISSUER, AUDIENCE, StsWebTokenFactory.tags(NS, SA, null, POD));
        AgentIdentity id = verifier(o).verify(jwt);
        assertNotNull(id);
        assertEquals(AGENT_ID, id.agentId());
        assertEquals("eks/agent/" + AGENT_ID, id.subject());
    }

    @Test
    void wrongIssuer_isRejected() throws Exception {
        String jwt = f.token("https://attacker.example/oidc", AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void wrongAudience_isRejected() throws Exception {
        String jwt = f.token(ISSUER, "some-other-aud",
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void wrongServiceAccount_isRejected() throws Exception {
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, "impostor-sa", CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void wrongNamespace_isRejected() throws Exception {
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags("kube-system", SA, CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void wrongClusterArn_isRejected() throws Exception {
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA,
                        "arn:aws:eks:us-east-1:999:cluster/other", POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void expiredToken_isRejected() throws Exception {
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD),
                new Date(System.currentTimeMillis() - 600_000));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void foreignKeyToken_isRejected() throws Exception {
        // Signed by a key not in the served JWKS ⇒ bad signature.
        StsWebTokenFactory foreign = StsWebTokenFactory.foreign();
        String jwt = foreign.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void algNoneToken_isRejected() throws Exception {
        String jwt = f.algNoneToken(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void hs256ConfusionToken_isRejected() throws Exception {
        // HS256 signed with the RSA public-key bytes; the JWKS key is RSA, so it
        // must never be treated as an HMAC secret.
        String jwt = f.hs256ConfusionToken(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, POD));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void missingPrincipalTags_isRejected() throws Exception {
        // Token has the namespaced claim but no principal_tags (pod not under
        // EKS Pod Identity) ⇒ null.
        StsWebOptions o = new StsWebOptions(jwksUrl, ISSUER, AUDIENCE, "", "", "", "");
        String jwt = f.tokenWithoutPrincipalTags(ISSUER, AUDIENCE);
        assertNull(verifier(o).verify(jwt));
    }

    @Test
    void missingPodNameTag_isRejected() throws Exception {
        // principal_tags present but no kubernetes-pod-name ⇒ no agentId ⇒ null.
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, null));
        assertNull(verifier(opts()).verify(jwt));
    }

    @Test
    void garbageCredential_isRejected() {
        assertNull(verifier(opts()).verify(null));
        assertNull(verifier(opts()).verify(""));
        assertNull(verifier(opts()).verify("not-a-jwt"));
    }

    // ── spoof resistance: forged request_tags cannot override attested pod name ──

    @Test
    void forgedPodName_ignored_realAgentIdDerived() throws Exception {
        // Mirrors Go TestVerify_IgnoresForgedRequestTags: the verifier reads only
        // principal_tags (EKS-attested), never a caller-forgeable sibling. Here we
        // simply confirm the derived agentId comes from the attested pod name; a
        // caller cannot get principal_tags signed at all (only EKS can).
        String jwt = f.token(ISSUER, AUDIENCE,
                StsWebTokenFactory.tags(NS, SA, CLUSTER, "agent-real-pod"));
        AgentIdentity id = verifier(opts()).verify(jwt);
        assertNotNull(id);
        assertEquals("agent-real", id.agentId());
    }

    // ── config-only checks disabled when unset (parity with Matches/want=="") ──

    @Test
    void unsetChecks_areNotEnforced() throws Exception {
        // All expected-context fields empty ⇒ only signature/lifetime matter;
        // any namespace/SA/cluster is accepted (parity with Go want=="" skip).
        StsWebOptions o = new StsWebOptions(jwksUrl, "", "", "", "", "", "");
        String jwt = f.token("https://whatever", "anything",
                StsWebTokenFactory.tags("some-ns", "some-sa", CLUSTER, POD));
        AgentIdentity id = verifier(o).verify(jwt);
        assertNotNull(id);
        assertEquals(AGENT_ID, id.agentId());
    }
}
