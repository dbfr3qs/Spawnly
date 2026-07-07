package run.spawnly.keycloak.registry;

import org.jboss.logging.Logger;
import org.keycloak.util.JsonSerialization;

import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.function.Supplier;

/**
 * Java port of identityserver/AgentRegistryClient.cs. The IdP's window onto the
 * registry's neutral HTTP contract: agent liveness (Phase 2), the delegation
 * policy lookup (Phase 3), and the consent endpoints (Phase 4).
 *
 * <p>DEPENDENCY DIRECTION: this depends only on the registry's HTTP surface, the
 * same one the Go control-plane callers use (internal/controlplane) — never the
 * reverse. It carries the control-plane bearer exactly the way the Go side does
 * ({@link ControlPlaneBearer}), selected by the same {@code CONTROL_PLANE_AUTH}
 * env so both ends agree.
 *
 * <p>FAIL CLOSED: every registry error (unreachable, non-2xx, malformed body)
 * surfaces as {@code null}/{@code false}, never a thrown exception — the caller
 * (the liveness gate) then rejects the token. A registry outage can never
 * fail-open into minting a token for an unverified/inactive agent.
 *
 * <p>Explicit HTTP timeouts (connect + per-request) so a hung registry socket
 * can never wedge the token endpoint, matching the SpireSvidValidator's bounded
 * client and the Go clients' timeouts.
 */
public class AgentRegistryClient {

    public static final String DEFAULT_REGISTRY_URL = "http://registry:8080";

    private static final Logger log = Logger.getLogger(AgentRegistryClient.class);
    private static final Duration REQUEST_TIMEOUT = Duration.ofSeconds(5);
    private static final Duration CONNECT_TIMEOUT = Duration.ofSeconds(5);

    private final String baseUrl;
    private final HttpClient http;
    private final Supplier<String> bearer;

    /** Production ctor: reads REGISTRY_URL + CONTROL_PLANE_* from the environment. */
    public AgentRegistryClient() {
        this(envOrDefault("REGISTRY_URL", DEFAULT_REGISTRY_URL),
                HttpClient.newBuilder().connectTimeout(CONNECT_TIMEOUT).build(),
                ControlPlaneBearer.fromEnv());
    }

    /** Test seam: inject the base URL, HTTP client (e.g. WireMock), and bearer supplier. */
    AgentRegistryClient(String baseUrl, HttpClient http, Supplier<String> bearer) {
        // Trim a trailing slash so "base/" + "/v1/..." never doubles the slash.
        this.baseUrl = baseUrl.endsWith("/") ? baseUrl.substring(0, baseUrl.length() - 1) : baseUrl;
        this.http = http;
        this.bearer = bearer;
    }

    /**
     * Fetches the full agent record. Returns {@code null} if the agent does not
     * exist, or the registry is unreachable/errors (fail closed), mirroring the
     * C# {@code GetAgent}.
     */
    public AgentRecord getAgent(String agentId) {
        String body = get("/v1/agents/" + enc(agentId));
        if (body == null) {
            return null;
        }
        try {
            return JsonSerialization.readValue(body, AgentRecord.class);
        } catch (Exception e) {
            log.debugf(e, "malformed agent record for %s", agentId);
            return null;
        }
    }

    /**
     * Fetches an organization record by id (Phase 9 per-org verifier resolution),
     * via the control-plane-gated {@code GET /v1/orgs/{id}}. Returns {@code null}
     * if the org does not exist, or the registry is unreachable/errors (fail
     * closed) — the caller then treats the org as unanchored and, for an STS
     * credential, refuses (never mint against an unknown anchor).
     */
    public Organization getOrg(String orgId) {
        String body = get("/v1/orgs/" + enc(orgId));
        if (body == null) {
            return null;
        }
        try {
            return JsonSerialization.readValue(body, Organization.class);
        } catch (Exception e) {
            log.debugf(e, "malformed org record for %s", orgId);
            return null;
        }
    }

    /**
     * Fetches the delegation policy for a (parentType → childType) edge (Phase 3).
     * Returns {@code null} only when the registry is unreachable or non-2xx; a
     * well-formed 200 is returned as-is (which may carry {@code allowed=false}),
     * mirroring the C# {@code GetDelegationPolicy}.
     */
    public DelegationPolicy getDelegationPolicy(String parentType, String childType) {
        String body = get("/v1/delegation-policy?parentType=" + enc(parentType) + "&childType=" + enc(childType));
        if (body == null) {
            return null;
        }
        try {
            return JsonSerialization.readValue(body, DelegationPolicy.class);
        } catch (Exception e) {
            log.debugf(e, "malformed delegation policy for %s -> %s", parentType, childType);
            return null;
        }
    }

    /**
     * Asks the registry whether a stored consent covers a spawn edge with the
     * requested scopes right now (Phase 4). Null only when the registry is
     * unreachable; callers treat null as "not granted". Mirrors C# {@code CheckConsent}.
     */
    public ConsentDecision checkConsent(String userId, String parentType, String childType, List<String> scopes) {
        StringBuilder q = new StringBuilder("/v1/consents/check?userId=").append(enc(userId))
                .append("&parentType=").append(enc(parentType))
                .append("&childType=").append(enc(childType));
        for (String s : scopes) {
            q.append("&scope=").append(enc(s));
        }
        String body = get(q.toString());
        if (body == null) {
            return null;
        }
        try {
            return JsonSerialization.readValue(body, ConsentDecision.class);
        } catch (Exception e) {
            log.debugf(e, "malformed consent decision");
            return null;
        }
    }

    // ── Consent requests (Phase 4 CIBA) ─────────────────────────────────────────

    /**
     * Creates (or fetches the existing open) pending consent request for a spawn
     * edge (Phase 4). Returns the {@link ConsentRequest} as-is — its status may
     * already be {@code approved} if the registry short-circuited on a covering
     * stored consent. Null only when the registry is unreachable or errors,
     * mirroring the C# {@code CreateConsentRequest}.
     *
     * <p>The registry keys the reuse on (userId, parentType, childType), so a
     * duplicate backchannel request for the same edge returns the same open
     * request rather than a new one.
     */
    public ConsentRequest createConsentRequest(String userId, String parentType, String childType,
                                               List<String> scopes, String bindingMessage,
                                               String externalRef, String agentId) {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("userId", userId);
        body.put("parentType", parentType);
        body.put("childType", childType);
        body.put("agentId", agentId);
        body.put("scopes", scopes);
        body.put("bindingMessage", bindingMessage);
        body.put("externalRef", externalRef);
        return postForConsentRequest("/v1/consent-requests", body);
    }

    /**
     * Fetches a single consent request by id (used by the completion poller to
     * learn the registry's decision). Null when not found or unreachable.
     * Mirrors the C# {@code GetConsentRequest}.
     */
    public ConsentRequest getConsentRequest(String id) {
        String responseBody = get("/v1/consent-requests/" + enc(id));
        if (responseBody == null) {
            return null;
        }
        try {
            return JsonSerialization.readValue(responseBody, ConsentRequest.class);
        } catch (Exception e) {
            log.debugf(e, "malformed consent request %s", id);
            return null;
        }
    }

    /**
     * Approves a pending consent request, recording the durable ConsentRecord in
     * the registry and sweeping any other open requests for the same edge. Null
     * only when the registry is unreachable or errors. Mirrors the C#
     * {@code ApproveConsentRequest}.
     */
    public ConsentRequest approveConsentRequest(String id, List<String> scopes) {
        Map<String, Object> body = new LinkedHashMap<>();
        body.put("scopes", scopes);
        return postForConsentRequest("/v1/consent-requests/" + enc(id) + "/approve", body);
    }

    /**
     * Denies a pending consent request. Null only when the registry is
     * unreachable or errors. Mirrors the C# {@code DenyConsentRequest}.
     */
    public ConsentRequest denyConsentRequest(String id) {
        return postForConsentRequest("/v1/consent-requests/" + enc(id) + "/deny", null);
    }

    /**
     * POSTs {@code body} as JSON and parses the response as a {@link ConsentRequest}.
     * Null on any failure (fail closed). Never throws.
     */
    private ConsentRequest postForConsentRequest(String path, Map<String, Object> body) {
        String responseBody = post(path, body);
        if (responseBody == null) {
            return null;
        }
        try {
            return JsonSerialization.readValue(responseBody, ConsentRequest.class);
        } catch (Exception e) {
            log.debugf(e, "malformed consent request response from %s", path);
            return null;
        }
    }

    // ── HTTP plumbing ─────────────────────────────────────────────────────────

    /**
     * Executes a POST with an optional JSON body, returning the response body on
     * 2xx or {@code null} on any failure (non-2xx, timeout, connection error).
     * Never throws — a registry outage fails closed. Carries the control-plane
     * bearer the same way {@link #get}.
     */
    private String post(String path, Map<String, Object> body) {
        try {
            HttpRequest.BodyPublisher pub;
            if (body == null) {
                pub = HttpRequest.BodyPublishers.noBody();
            } else {
                pub = HttpRequest.BodyPublishers.ofByteArray(JsonSerialization.writeValueAsBytes(body));
            }
            HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(baseUrl + path))
                    .timeout(REQUEST_TIMEOUT)
                    .header("Content-Type", "application/json")
                    .POST(pub);
            String token = bearer.get();
            if (token != null && !token.isEmpty()) {
                b.header("Authorization", "Bearer " + token);
            }
            HttpResponse<String> resp = http.send(b.build(), HttpResponse.BodyHandlers.ofString());
            int code = resp.statusCode();
            if (code < 200 || code >= 300) {
                log.debugf("registry POST %s returned %d", path, code);
                return null;
            }
            return resp.body();
        } catch (Exception e) {
            log.debugf(e, "registry POST %s failed", path);
            return null;
        }
    }

    /**
     * Executes a GET, returning the response body on 2xx or {@code null} on any
     * failure (non-2xx, timeout, connection error). Never throws. The
     * control-plane bearer, if present, is attached the same way the Go
     * bearerTransport does.
     */
    private String get(String path) {
        try {
            HttpRequest.Builder b = HttpRequest.newBuilder(URI.create(baseUrl + path))
                    .timeout(REQUEST_TIMEOUT)
                    .GET();
            String token = bearer.get();
            if (token != null && !token.isEmpty()) {
                b.header("Authorization", "Bearer " + token);
            }
            HttpResponse<String> resp = http.send(b.build(), HttpResponse.BodyHandlers.ofString());
            int code = resp.statusCode();
            if (code < 200 || code >= 300) {
                log.debugf("registry GET %s returned %d", path, code);
                return null;
            }
            return resp.body();
        } catch (Exception e) {
            // Unreachable / timeout / interrupted ⇒ fail closed.
            log.debugf(e, "registry GET %s failed", path);
            return null;
        }
    }

    private static String enc(String s) {
        return URLEncoder.encode(s == null ? "" : s, StandardCharsets.UTF_8);
    }

    static String envOrDefault(String key, String def) {
        String v = System.getenv(key);
        return (v == null || v.isEmpty()) ? def : v;
    }
}
