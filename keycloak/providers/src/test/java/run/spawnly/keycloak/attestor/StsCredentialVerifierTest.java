package run.spawnly.keycloak.attestor;

import com.github.tomakehurst.wiremock.WireMockServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.util.Base64;

import static com.github.tomakehurst.wiremock.client.WireMock.aResponse;
import static com.github.tomakehurst.wiremock.client.WireMock.any;
import static com.github.tomakehurst.wiremock.client.WireMock.anyUrl;
import static com.github.tomakehurst.wiremock.core.WireMockConfiguration.options;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Ports the identityserver StsCredentialVerifier behaviour ({@code aws-sts}) and
 * verifies parity with the Go registrant verifier
 * ({@code internal/attestor/aws_verify.go} + {@code internal/registrant/aws.go}):
 * the presigned URL is validated (STS host + https + GetCallerIdentity — SSRF
 * guard), the response ARN is parsed, and agentId = the assumed-role session
 * name, subject = the full ARN, issuer = "aws-sts".
 *
 * <p>Like the Go test ({@code TestVerifyGetCallerIdentity}), the replay+parse
 * happy path is exercised via {@link StsCredentialVerifier#replay} against a
 * mock endpoint, because the full {@link StsCredentialVerifier#verify} path's
 * SSRF guard (correctly) rejects any non-STS host — including localhost.
 */
class StsCredentialVerifierTest {

    // A canonical assumed-role ARN; the session name IS the agentId (the
    // operator sets RoleSessionName = agentId). Shared with the Go vectors.
    private static final String ARN =
            "arn:aws:sts::123456789012:assumed-role/spawnly-agent/worker-ab12cd";
    private static final String GCI_XML =
            "<GetCallerIdentityResponse xmlns=\"https://sts.amazonaws.com/doc/2011-06-15/\">"
                    + "<GetCallerIdentityResult><Arn>" + ARN + "</Arn>"
                    + "<UserId>AROA:worker-ab12cd</UserId><Account>123456789012</Account>"
                    + "</GetCallerIdentityResult></GetCallerIdentityResponse>";

    private WireMockServer wm;

    @BeforeEach
    void setUp() {
        wm = new WireMockServer(options().dynamicPort());
        wm.start();
    }

    @AfterEach
    void tearDown() {
        if (wm != null) wm.stop();
    }

    private StsCredentialVerifier verifier() {
        return new StsCredentialVerifier(HttpClient.newHttpClient());
    }

    private static String cred(String method, String url) {
        String json = "{\"method\":\"" + method + "\",\"url\":\"" + url + "\"}";
        return Base64.getEncoder().encodeToString(json.getBytes(StandardCharsets.UTF_8));
    }

    // ── replay + parse (mirrors Go replayGetCallerIdentity happy path) ──

    @Test
    void replay_validArn_returnsArn() {
        wm.stubFor(any(anyUrl()).willReturn(aResponse()
                .withHeader("Content-Type", "text/xml").withBody(GCI_XML)));
        String url = "http://localhost:" + wm.port() + "/?Action=GetCallerIdentity";
        String arn = verifier().replay(new StsCredentialVerifier.PresignedRequest("GET", url));
        assertEquals(ARN, arn);
        // And the full identity derivation off that ARN:
        assertEquals("worker-ab12cd", StsCredentialVerifier.sessionNameFromArn(arn));
    }

    @Test
    void replay_non200_isRejected() {
        wm.stubFor(any(anyUrl()).willReturn(aResponse().withStatus(403).withBody("Forbidden")));
        String url = "http://localhost:" + wm.port() + "/?Action=GetCallerIdentity";
        assertNull(verifier().replay(new StsCredentialVerifier.PresignedRequest("GET", url)));
    }

    @Test
    void replay_malformedArn_isRejected() {
        // A shape with no <Arn> element ⇒ null (no identity to derive).
        wm.stubFor(any(anyUrl()).willReturn(aResponse().withHeader("Content-Type", "text/xml")
                .withBody("<GetCallerIdentityResponse><GetCallerIdentityResult>"
                        + "<Account>123456789012</Account></GetCallerIdentityResult>"
                        + "</GetCallerIdentityResponse>")));
        String url = "http://localhost:" + wm.port() + "/?Action=GetCallerIdentity";
        assertNull(verifier().replay(new StsCredentialVerifier.PresignedRequest("GET", url)));
    }

    // ── SSRF guard: verify() rejects non-STS / tampered URLs before any call ──

    @Test
    void verify_nonStsHost_isRejected_noHttpCall() {
        // Points at WireMock (localhost, non-STS). The host guard must reject it
        // WITHOUT issuing the request — otherwise it is an SSRF sink.
        String url = "https://localhost:" + wm.port() + "/?Action=GetCallerIdentity";
        assertNull(verifier().verify(cred("GET", url)));
        assertFalse(wm.getServeEvents().getRequests().size() > 0, "no request should be issued to a non-STS host");
    }

    @Test
    void validateStsUrl_sharedVectorsWithGo() {
        // Mirror internal/attestor/aws_test.go:TestValidateStsURL exactly.
        assertTrue(StsCredentialVerifier.validateStsUrl(
                "https://sts.amazonaws.com/?Action=GetCallerIdentity"));
        assertTrue(StsCredentialVerifier.validateStsUrl(
                "https://sts.us-east-1.amazonaws.com/?Action=GetCallerIdentity"));
        assertFalse(StsCredentialVerifier.validateStsUrl(
                "http://sts.amazonaws.com/?Action=GetCallerIdentity")); // not https
        assertFalse(StsCredentialVerifier.validateStsUrl(
                "https://evil.example.com/?Action=GetCallerIdentity")); // not STS
        assertFalse(StsCredentialVerifier.validateStsUrl(
                "https://sts.amazonaws.com/?Action=AssumeRole")); // wrong action
        assertFalse(StsCredentialVerifier.validateStsUrl(
                "https://sts.amazonaws.com.evil.com/?Action=GetCallerIdentity")); // suffix attack
    }

    @Test
    void verify_garbageCredential_isRejected() {
        assertNull(verifier().verify(null));
        assertNull(verifier().verify(""));
        assertNull(verifier().verify("not-base64!!!"));
        // Valid base64 but not a presigned-request JSON object.
        assertNull(verifier().verify(Base64.getEncoder()
                .encodeToString("just a string".getBytes(StandardCharsets.UTF_8))));
        // Presigned JSON missing url/method.
        assertNull(verifier().verify(Base64.getEncoder()
                .encodeToString("{\"headers\":{}}".getBytes(StandardCharsets.UTF_8))));
    }

    // ── agentId / subject derivation vectors — MUST match the registry ──
    // (internal/attestor/aws_verify.go: SessionNameFromArn; registrant/aws.go)

    @Test
    void sessionNameFromArn_sharedVectorsWithGo() {
        // The happy vector from Go TestSessionNameFromArn.
        assertEquals("worker-ab12cd", StsCredentialVerifier.sessionNameFromArn(
                "arn:aws:sts::123456789012:assumed-role/spawnly-agent/worker-ab12cd"));
        // A non-assumed-role ARN has no session name ⇒ null (Go returns error).
        assertNull(StsCredentialVerifier.sessionNameFromArn(
                "arn:aws:iam::123456789012:user/bob"));
        // assumed-role with a role but empty/absent session name ⇒ null.
        assertNull(StsCredentialVerifier.sessionNameFromArn(
                "arn:aws:sts::123456789012:assumed-role/only-role"));
        assertNull(StsCredentialVerifier.sessionNameFromArn(
                "arn:aws:sts::123456789012:assumed-role/only-role/"));
        assertNull(StsCredentialVerifier.sessionNameFromArn(null));
        // A session name containing slashes is preserved after the first slash
        // (parity with Go SplitN(rest, "/", 2)).
        assertEquals("multi/segment/name", StsCredentialVerifier.sessionNameFromArn(
                "arn:aws:sts::1:assumed-role/role/multi/segment/name"));
    }

    @Test
    void parseArn_ignoresNamespace() {
        // The real STS response carries an XML namespace; matching by local name
        // recovers the ARN regardless (parity with the C# LocalName match).
        assertEquals(ARN, StsCredentialVerifier.parseArn(GCI_XML));
        assertNull(StsCredentialVerifier.parseArn("<x/>"));
        assertNull(StsCredentialVerifier.parseArn("not xml at all <<<"));
    }

    @Test
    void parseArn_rejectsXxe() {
        // DTDs are disabled, so an external-entity payload does not resolve — the
        // parser fails closed (null) rather than fetching the entity.
        String xxe = "<?xml version=\"1.0\"?>"
                + "<!DOCTYPE r [<!ENTITY x SYSTEM \"file:///etc/hostname\">]>"
                + "<GetCallerIdentityResult><Arn>&x;</Arn></GetCallerIdentityResult>";
        assertNull(StsCredentialVerifier.parseArn(xxe));
    }
}
