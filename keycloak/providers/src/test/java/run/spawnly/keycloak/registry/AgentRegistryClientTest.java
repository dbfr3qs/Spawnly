package run.spawnly.keycloak.registry;

import com.github.tomakehurst.wiremock.WireMockServer;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import java.net.http.HttpClient;
import java.time.Duration;
import java.util.List;

import static com.github.tomakehurst.wiremock.client.WireMock.aResponse;
import static com.github.tomakehurst.wiremock.client.WireMock.equalTo;
import static com.github.tomakehurst.wiremock.client.WireMock.get;
import static com.github.tomakehurst.wiremock.client.WireMock.getRequestedFor;
import static com.github.tomakehurst.wiremock.client.WireMock.matchingJsonPath;
import static com.github.tomakehurst.wiremock.client.WireMock.post;
import static com.github.tomakehurst.wiremock.client.WireMock.postRequestedFor;
import static com.github.tomakehurst.wiremock.client.WireMock.urlEqualTo;
import static com.github.tomakehurst.wiremock.client.WireMock.urlPathEqualTo;
import static com.github.tomakehurst.wiremock.client.WireMock.absent;
import static com.github.tomakehurst.wiremock.core.WireMockConfiguration.options;
import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * Ports the identityserver/AgentRegistryClient.cs behaviour and verifies parity:
 * agent lookup shape, delegation-policy + consent parsing, the control-plane
 * bearer being carried the Go way, and FAIL-CLOSED on every registry error
 * (unreachable, non-2xx, malformed body) → null/false.
 */
class AgentRegistryClientTest {

    private WireMockServer wm;
    private String baseUrl;

    @BeforeEach
    void setUp() {
        wm = new WireMockServer(options().dynamicPort());
        wm.start();
        baseUrl = "http://localhost:" + wm.port();
    }

    @AfterEach
    void tearDown() {
        wm.stop();
    }

    private AgentRegistryClient client(java.util.function.Supplier<String> bearer) {
        return new AgentRegistryClient(baseUrl,
                HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).build(),
                bearer);
    }

    @Test
    void getAgent_parsesRecord() {
        wm.stubFor(get(urlEqualTo("/v1/agents/agent-abc")).willReturn(aResponse()
                .withHeader("Content-Type", "application/json")
                .withBody("{\"agentId\":\"agent-abc\",\"status\":\"active\",\"userId\":\"alice\","
                        + "\"agentType\":\"weather-monitor\",\"parentId\":null,\"extraFutureField\":42}")));

        AgentRecord rec = client(() -> "").getAgent("agent-abc");

        assertNotNull(rec);
        assertEquals("agent-abc", rec.agentId());
        assertEquals("active", rec.status());
        assertEquals("alice", rec.userId());
        assertEquals("weather-monitor", rec.agentType());
        // Org-less record (no orgId field over the wire) ⇒ orgId is null. This is
        // the forward-compat path: a pre-org registry record parses cleanly.
        assertNull(rec.orgId());
    }

    @Test
    void getAgent_parsesOrgId() {
        // Multi-tenancy hinges on the org coming from the registry RECORD over the
        // wire (the IdP never trusts a request token for it), so pin that the
        // JSON `orgId` field deserializes onto AgentRecord.orgId().
        wm.stubFor(get(urlEqualTo("/v1/agents/agent-org")).willReturn(aResponse()
                .withHeader("Content-Type", "application/json")
                .withBody("{\"agentId\":\"agent-org\",\"status\":\"active\",\"userId\":\"alice\","
                        + "\"agentType\":\"weather-monitor\",\"parentId\":null,\"orgId\":\"acme\"}")));

        AgentRecord rec = client(() -> "").getAgent("agent-org");

        assertNotNull(rec);
        assertEquals("acme", rec.orgId());
    }

    @Test
    void getAgent_notFound_returnsNull() {
        wm.stubFor(get(urlEqualTo("/v1/agents/ghost")).willReturn(aResponse().withStatus(404)));
        assertNull(client(() -> "").getAgent("ghost"));
    }

    @Test
    void getAgent_serverError_failsClosedNull() {
        wm.stubFor(get(urlEqualTo("/v1/agents/x")).willReturn(aResponse().withStatus(500)));
        assertNull(client(() -> "").getAgent("x"));
    }

    @Test
    void getAgent_malformedBody_failsClosedNull() {
        wm.stubFor(get(urlEqualTo("/v1/agents/x")).willReturn(aResponse()
                .withHeader("Content-Type", "application/json")
                .withBody("not json")));
        assertNull(client(() -> "").getAgent("x"));
    }

    @Test
    void getAgent_unreachable_failsClosedNull() {
        wm.stop(); // nothing listening
        assertNull(client(() -> "").getAgent("x"));
    }

    @Test
    void getAgent_urlEncodesId() {
        wm.stubFor(get(urlEqualTo("/v1/agents/a%2Fb")).willReturn(aResponse()
                .withBody("{\"agentId\":\"a/b\",\"status\":\"active\",\"userId\":\"u\",\"agentType\":\"t\"}")));
        AgentRecord rec = client(() -> "").getAgent("a/b");
        assertNotNull(rec);
        assertEquals("a/b", rec.agentId());
    }

    @Test
    void sharedSecretBearer_isCarried() {
        wm.stubFor(get(urlEqualTo("/v1/agents/agent-abc")).willReturn(aResponse()
                .withBody("{\"agentId\":\"agent-abc\",\"status\":\"active\",\"userId\":\"u\",\"agentType\":\"t\"}")));

        client(() -> "s3cret").getAgent("agent-abc");

        wm.verify(getRequestedFor(urlEqualTo("/v1/agents/agent-abc"))
                .withHeader("Authorization", equalTo("Bearer s3cret")));
    }

    @Test
    void emptyBearer_sendsNoAuthHeader() {
        wm.stubFor(get(urlEqualTo("/v1/agents/agent-abc")).willReturn(aResponse()
                .withBody("{\"agentId\":\"agent-abc\",\"status\":\"active\",\"userId\":\"u\",\"agentType\":\"t\"}")));

        client(() -> "").getAgent("agent-abc");

        wm.verify(getRequestedFor(urlEqualTo("/v1/agents/agent-abc"))
                .withHeader("Authorization", absent()));
    }

    @Test
    void getDelegationPolicy_parsesAndCarriesQuery() {
        wm.stubFor(get(urlPathEqualTo("/v1/delegation-policy")).willReturn(aResponse()
                .withBody("{\"allowed\":true,\"grantableScopes\":[\"flights:read\",\"hotels:read\"],\"maxDepth\":3}")));

        DelegationPolicy p = client(() -> "").getDelegationPolicy("travel-planner", "flight-search");

        assertNotNull(p);
        assertTrue(p.allowed());
        assertEquals(List.of("flights:read", "hotels:read"), p.grantableScopes());
        assertEquals(3, p.maxDepth());
    }

    @Test
    void getDelegationPolicy_notAllowedIsReturnedAsIs() {
        wm.stubFor(get(urlPathEqualTo("/v1/delegation-policy")).willReturn(aResponse()
                .withBody("{\"allowed\":false,\"grantableScopes\":[],\"maxDepth\":0}")));
        DelegationPolicy p = client(() -> "").getDelegationPolicy("a", "b");
        assertNotNull(p);
        assertFalse(p.allowed());
    }

    @Test
    void getDelegationPolicy_error_failsClosedNull() {
        wm.stubFor(get(urlPathEqualTo("/v1/delegation-policy")).willReturn(aResponse().withStatus(503)));
        assertNull(client(() -> "").getDelegationPolicy("a", "b"));
    }

    @Test
    void checkConsent_parsesDecisionAndEmitsScopes() {
        wm.stubFor(get(urlPathEqualTo("/v1/consents/check")).willReturn(aResponse()
                .withBody("{\"granted\":true,\"reason\":\"stored\"}")));

        ConsentDecision d = client(() -> "").checkConsent("alice", "p", "c", List.of("flights:read", "fx:read"));

        assertNotNull(d);
        assertTrue(d.granted());
        assertEquals("stored", d.reason());
    }

    @Test
    void checkConsent_error_failsClosedNull() {
        wm.stubFor(get(urlPathEqualTo("/v1/consents/check")).willReturn(aResponse().withStatus(500)));
        assertNull(client(() -> "").checkConsent("u", "p", "c", List.of("s")));
    }

    // ── Consent requests (Phase 4 CIBA) ─────────────────────────────────────────

    @Test
    void createConsentRequest_postsBodyAndParsesPending() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests")).willReturn(aResponse()
                .withHeader("Content-Type", "application/json")
                .withBody("{\"id\":\"cr-1\",\"userId\":\"alice\",\"parentType\":\"travel-planner\","
                        + "\"childType\":\"flight-search\",\"agentId\":\"agent-x\",\"scopes\":[\"flights:read\"],"
                        + "\"bindingMessage\":\"flight-search requests flights:read\",\"status\":\"pending\","
                        + "\"externalRef\":\"authres-1\",\"future\":1}")));

        ConsentRequest cr = client(() -> "").createConsentRequest(
                "alice", "travel-planner", "flight-search", List.of("flights:read"),
                "flight-search requests flights:read", "authres-1", "agent-x");

        assertNotNull(cr);
        assertEquals("cr-1", cr.id());
        assertEquals("alice", cr.userId());
        assertEquals("travel-planner", cr.parentType());
        assertEquals("flight-search", cr.childType());
        assertTrue(cr.isPending());
        assertFalse(cr.isApproved());

        // Body carries the edge fields (bound from the SVID, not client hints).
        wm.verify(postRequestedFor(urlEqualTo("/v1/consent-requests"))
                .withRequestBody(matchingJsonPath("$.userId", equalTo("alice")))
                .withRequestBody(matchingJsonPath("$.parentType", equalTo("travel-planner")))
                .withRequestBody(matchingJsonPath("$.childType", equalTo("flight-search")))
                .withRequestBody(matchingJsonPath("$.agentId", equalTo("agent-x")))
                .withRequestBody(matchingJsonPath("$.externalRef", equalTo("authres-1"))));
    }

    @Test
    void createConsentRequest_returnsApprovedWhenRegistryShortCircuits() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests")).willReturn(aResponse()
                .withBody("{\"id\":\"cr-2\",\"userId\":\"alice\",\"parentType\":\"p\",\"childType\":\"c\","
                        + "\"status\":\"approved\"}")));

        ConsentRequest cr = client(() -> "").createConsentRequest(
                "alice", "p", "c", List.of("s"), null, "ref", "agent-y");

        assertNotNull(cr);
        assertTrue(cr.isApproved());
    }

    @Test
    void createConsentRequest_error_failsClosedNull() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests")).willReturn(aResponse().withStatus(503)));
        assertNull(client(() -> "").createConsentRequest("u", "p", "c", List.of("s"), null, "r", "a"));
    }

    @Test
    void getConsentRequest_parsesStatus() {
        wm.stubFor(get(urlEqualTo("/v1/consent-requests/cr-1")).willReturn(aResponse()
                .withBody("{\"id\":\"cr-1\",\"userId\":\"alice\",\"parentType\":\"p\",\"childType\":\"c\","
                        + "\"status\":\"denied\"}")));
        ConsentRequest cr = client(() -> "").getConsentRequest("cr-1");
        assertNotNull(cr);
        assertTrue(cr.isDenied());
    }

    @Test
    void getConsentRequest_error_failsClosedNull() {
        wm.stubFor(get(urlEqualTo("/v1/consent-requests/cr-1")).willReturn(aResponse().withStatus(500)));
        assertNull(client(() -> "").getConsentRequest("cr-1"));
    }

    @Test
    void approveConsentRequest_postsScopesAndCarriesBearer() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests/cr-1/approve")).willReturn(aResponse()
                .withBody("{\"id\":\"cr-1\",\"userId\":\"alice\",\"parentType\":\"p\",\"childType\":\"c\","
                        + "\"status\":\"approved\"}")));

        ConsentRequest cr = client(() -> "cp-token").approveConsentRequest("cr-1", List.of("flights:read"));

        assertNotNull(cr);
        assertTrue(cr.isApproved());
        wm.verify(postRequestedFor(urlEqualTo("/v1/consent-requests/cr-1/approve"))
                .withHeader("Authorization", equalTo("Bearer cp-token"))
                .withRequestBody(matchingJsonPath("$.scopes[0]", equalTo("flights:read"))));
    }

    @Test
    void approveConsentRequest_error_failsClosedNull() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests/cr-1/approve")).willReturn(aResponse().withStatus(500)));
        assertNull(client(() -> "").approveConsentRequest("cr-1", List.of("s")));
    }

    @Test
    void denyConsentRequest_postsNoBody() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests/cr-1/deny")).willReturn(aResponse()
                .withBody("{\"id\":\"cr-1\",\"userId\":\"alice\",\"parentType\":\"p\",\"childType\":\"c\","
                        + "\"status\":\"denied\"}")));
        ConsentRequest cr = client(() -> "").denyConsentRequest("cr-1");
        assertNotNull(cr);
        assertTrue(cr.isDenied());
        wm.verify(postRequestedFor(urlEqualTo("/v1/consent-requests/cr-1/deny")));
    }

    @Test
    void denyConsentRequest_error_failsClosedNull() {
        wm.stubFor(post(urlEqualTo("/v1/consent-requests/cr-1/deny")).willReturn(aResponse().withStatus(500)));
        assertNull(client(() -> "").denyConsentRequest("cr-1"));
    }
}
