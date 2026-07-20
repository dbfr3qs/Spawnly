package run.spawnly.keycloak.attestor;

import com.github.tomakehurst.wiremock.WireMockServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.net.http.HttpClient;
import java.util.Date;

import static com.github.tomakehurst.wiremock.client.WireMock.aResponse;
import static com.github.tomakehurst.wiremock.client.WireMock.get;
import static com.github.tomakehurst.wiremock.client.WireMock.urlEqualTo;
import static com.github.tomakehurst.wiremock.core.WireMockConfiguration.options;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;

/**
 * Ports the identityserver SPIRE-validator behaviour and verifies parity:
 * valid / expired / garbage SVID, agentId = last SPIFFE path segment (the same
 * derivation the Go registry's registrant verifier uses, path.Base), and the
 * wrong-audience parity note (audience is NOT validated at this layer, matching
 * the C# ValidateAudience=false).
 */
class SpireCredentialVerifierTest {

    private WireMockServer wm;
    private SvidTestFactory f;
    private String jwksUrl;

    @BeforeEach
    void setUp() throws Exception {
        wm = new WireMockServer(options().dynamicPort());
        wm.start();
        f = new SvidTestFactory();
        wm.stubFor(get(urlEqualTo("/keys")).willReturn(aResponse()
                .withHeader("Content-Type", "application/json")
                .withBody(f.jwksJson())));
        jwksUrl = "http://localhost:" + wm.port() + "/keys";
    }

    @AfterEach
    void tearDown() {
        if (wm != null) wm.stop();
    }

    private SpireCredentialVerifier verifier() {
        // Plain-HTTP client so the WireMock (http) endpoint is reachable without
        // TLS; the production path uses the TLS-relaxed client. Legacy mode (no
        // trust-domain enforcement) — the pre-substrate behaviour these vectors pin.
        SpireSvidValidator v = new SpireSvidValidator(jwksUrl, HttpClient.newHttpClient());
        return new SpireCredentialVerifier(v);
    }

    // Enforcing verifier: trust-domain pinned to cluster.local + org parsing.
    private SpireCredentialVerifier enforcing() {
        SpireSvidValidator v = new SpireSvidValidator(jwksUrl, HttpClient.newHttpClient());
        return new SpireCredentialVerifier(v, "cluster.local");
    }

    @Test
    void validSvid_returnsIdentity_withLastSegmentAgentId() throws Exception {
        String spiffe = "spiffe://cluster.local/agent/acme/alice/weather-monitor/agent-abc123";
        String svid = f.svid(spiffe, "keys");

        AgentIdentity id = verifier().verify(svid);

        assertNotNull(id);
        assertEquals(spiffe, id.subject());
        assertEquals("agent-abc123", id.agentId());
        assertEquals("spiffe-svid", id.issuer());
    }

    @Test
    void expiredSvid_isRejected() throws Exception {
        String svid = f.svid("spiffe://cluster.local/agent/agent-x", "keys",
                new Date(System.currentTimeMillis() - 600_000)); // 10 min ago
        assertNull(verifier().verify(svid));
    }

    @Test
    void garbageSvid_isRejected() {
        assertNull(verifier().verify("not-a-jwt"));
        assertNull(verifier().verify(""));
        assertNull(verifier().verify(null));
    }

    @Test
    void svidSignedByForeignKey_isRejected() throws Exception {
        // Signed by a key NOT in the JWKS the validator fetches ⇒ bad signature.
        SvidTestFactory foreign = SvidTestFactory.foreign();
        String svid = foreign.svid("spiffe://cluster.local/agent/agent-x", "keys");
        assertNull(verifier().verify(svid));
    }

    @Test
    void wrongAudience_isAcceptedAtValidatorLayer_parityWithCSharp() throws Exception {
        // PARITY: identityserver/SpireSvidValidator.cs sets ValidateAudience=false,
        // so the SVID validator itself does NOT reject on audience — SPIRE issues
        // the SVID for the audience the sidecar requests and the client-auth path
        // has no single fixed audience. Documented here so the invariant is
        // explicit and any future audience tightening is a deliberate change.
        String spiffe = "spiffe://cluster.local/agent/agent-y";
        String svid = f.svid(spiffe, "some-other-audience");
        AgentIdentity id = verifier().verify(svid);
        assertNotNull(id);
        assertEquals(spiffe, id.subject());
    }

    @Test
    void algNoneSvid_isRejected() throws Exception {
        // alg=none (unsecured JWT) must never be accepted — no signature to trust.
        String svid = f.algNoneSvid("spiffe://cluster.local/agent/acme/alice/weather-monitor/evil");
        assertNull(verifier().verify(svid));
    }

    @Test
    void hs256ConfusionSvid_isRejected() throws Exception {
        // JWT algorithm-confusion: an HS256 token signed with the (public) RSA
        // key bytes as the HMAC secret. The JWKS key is RSA, so treating it as an
        // HMAC secret must fail — otherwise anyone who reads the public JWKS could
        // forge SVIDs.
        String svid = f.hs256ConfusionSvid("spiffe://cluster.local/agent/acme/alice/weather-monitor/evil");
        assertNull(verifier().verify(svid));
    }

    @Test
    void jwksFetchFails_isRejected() throws Exception {
        wm.stop(); // JWKS endpoint now unreachable
        String svid = f.svid("spiffe://cluster.local/agent/agent-x", "keys");
        assertNull(verifier().verify(svid));
    }

    // ── trust-domain enforcement + org parsing (remote-substrate Phase 1) ──

    @Test
    void enforcing_platformSvid_verifiesWithNoOrg() throws Exception {
        String spiffe = "spiffe://cluster.local/agent/acme/alice/weather-monitor/agent-abc";
        AgentIdentity id = enforcing().verify(f.svid(spiffe, "keys"));
        assertNotNull(id);
        assertEquals("agent-abc", id.agentId());
        assertNull(id.orgId()); // platform (org-less) path
    }

    @Test
    void enforcing_orgScopedSvid_surfacesOrg() throws Exception {
        String spiffe = "spiffe://cluster.local/org/acme/agent/t/alice/weather-monitor/agent-abc";
        AgentIdentity id = enforcing().verify(f.svid(spiffe, "keys"));
        assertNotNull(id);
        assertEquals("agent-abc", id.agentId()); // path.Base preserved
        assertEquals("acme", id.orgId());
    }

    @Test
    void enforcing_wrongTrustDomain_isRejected() throws Exception {
        String spiffe = "spiffe://evil.example/org/acme/agent/t/alice/weather-monitor/agent-abc";
        assertNull(enforcing().verify(f.svid(spiffe, "keys")));
    }

    @Test
    void enforcing_emptyOrgSpoof_isRejected() throws Exception {
        // "/org//agent/…" — an empty org value must never be smoothed over.
        String spiffe = "spiffe://cluster.local/org//agent/agent-abc";
        assertNull(enforcing().verify(f.svid(spiffe, "keys")));
    }

    @Test
    void enforcing_orgInSubpath_isPlatform_notAnOrgClaim() throws Exception {
        // "org" appearing deeper is NOT an org claim ⇒ platform (org null).
        String spiffe = "spiffe://cluster.local/agent/org/agent-abc";
        AgentIdentity id = enforcing().verify(f.svid(spiffe, "keys"));
        assertNotNull(id);
        assertEquals("agent-abc", id.agentId());
        assertNull(id.orgId());
    }

    @Test
    void legacyMode_orgScopedPath_isNotParsed() throws Exception {
        // No trust-domain configured ⇒ passthrough: agentId = path.Base, org null,
        // even for an org-scoped path (bit-identical to pre-substrate).
        String spiffe = "spiffe://example.org/org/acme/agent/t/u/worker/agent-abc";
        AgentIdentity id = verifier().verify(f.svid(spiffe, "keys"));
        assertNotNull(id);
        assertEquals("agent-abc", id.agentId());
        assertNull(id.orgId());
    }

    // ── agentId-derivation vectors — MUST match the registry (Go path.Base) ──

    @Test
    void agentIdDerivation_matchesRegistryPathBase() {
        // Vectors mirror internal/registrant/spiffe.go's path.Base(spiffeID).
        assertEquals("agent-abc123",
                SpireCredentialVerifier.lastPathSegment(
                        "spiffe://cluster.local/agent/acme/alice/weather-monitor/agent-abc123"));
        assertEquals("id",
                SpireCredentialVerifier.lastPathSegment("spiffe://td/agent/id"));
        // Trailing slash is stripped (path.Base semantics).
        assertEquals("id",
                SpireCredentialVerifier.lastPathSegment("spiffe://td/agent/id/"));
        assertEquals("legacy-agent",
                SpireCredentialVerifier.lastPathSegment("spiffe://cluster.local/agent/legacy-agent"));
    }
}
