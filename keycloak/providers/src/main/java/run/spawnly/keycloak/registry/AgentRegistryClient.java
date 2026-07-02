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
import java.util.List;
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

    // ── HTTP plumbing ─────────────────────────────────────────────────────────

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
