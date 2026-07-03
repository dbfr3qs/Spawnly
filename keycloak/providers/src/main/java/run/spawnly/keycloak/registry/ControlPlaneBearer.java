package run.spawnly.keycloak.registry;

import org.jboss.logging.Logger;
import org.keycloak.util.JsonSerialization;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.net.URI;
import java.net.URLEncoder;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.util.function.Supplier;

/**
 * Produces the control-plane bearer the IdP presents to the registry's
 * endpoints, selected by {@code CONTROL_PLANE_AUTH} exactly like the C#
 * {@code ControlPlaneTokenHandler} (identityserver/AgentRegistryClient.cs) and
 * the Go {@code controlplane.BearerSource} (internal/controlplane/bearer.go), so
 * all callers agree by construction:
 *
 * <ul>
 *   <li>{@code none}/unset → no header (local demo; registry runs open)</li>
 *   <li>{@code shared-secret} → the static {@code CONTROL_PLANE_TOKEN}</li>
 *   <li>{@code oidc} → a client-credentials access token (the {@code idp-consent}
 *       client), fetched and cached until shortly before expiry</li>
 * </ul>
 *
 * <p>The returned {@link Supplier} is safe to call per request: for oidc it
 * returns the current (refreshed) token, and on a refresh error it returns
 * {@code ""} so the registry fails the request closed rather than accepting a
 * stale token — matching the Go bearerTransport's fail-closed behaviour.
 */
public final class ControlPlaneBearer {

    private static final Logger log = Logger.getLogger(ControlPlaneBearer.class);

    private ControlPlaneBearer() {
    }

    /** Builds the bearer supplier from the environment. Never throws. */
    public static Supplier<String> fromEnv() {
        String auth = System.getenv("CONTROL_PLANE_AUTH");
        if (auth == null) {
            auth = "";
        }
        switch (auth) {
            case "":
            case "none":
                return () -> "";
            case "shared-secret": {
                String token = System.getenv("CONTROL_PLANE_TOKEN");
                if (token == null) {
                    token = "";
                }
                final String t = token;
                return () -> t;
            }
            case "oidc":
                return new OidcTokenSource(
                        envOr("CONTROL_PLANE_TOKEN_URL", ""),
                        envOr("CONTROL_PLANE_CLIENT_ID", ""),
                        envOr("CONTROL_PLANE_CLIENT_SECRET", ""),
                        envOr("CONTROL_PLANE_SCOPE", "registry.consent"),
                        HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(5)).build());
            default:
                log.warnf("unknown CONTROL_PLANE_AUTH '%s' — sending no control-plane bearer (fail closed)", auth);
                return () -> "";
        }
    }

    private static String envOr(String key, String def) {
        String v = System.getenv(key);
        return (v == null || v.isEmpty()) ? def : v;
    }

    /**
     * Fetches + caches a client-credentials token, refreshing ~30s before expiry.
     * Thread-safe (synchronized): the token endpoint may serve concurrent
     * requests, but a single cached token is fine to share.
     */
    static final class OidcTokenSource implements Supplier<String> {
        private final String tokenUrl;
        private final String clientId;
        private final String clientSecret;
        private final String scope;
        private final HttpClient http;

        private String cached;
        private Instant expiresAt = Instant.EPOCH;

        OidcTokenSource(String tokenUrl, String clientId, String clientSecret, String scope, HttpClient http) {
            this.tokenUrl = tokenUrl;
            this.clientId = clientId;
            this.clientSecret = clientSecret;
            this.scope = scope;
            this.http = http;
        }

        @Override
        public synchronized String get() {
            if (cached != null && Instant.now().isBefore(expiresAt)) {
                return cached;
            }
            if (tokenUrl.isEmpty() || clientId.isEmpty()) {
                log.warn("CONTROL_PLANE_AUTH=oidc but CONTROL_PLANE_TOKEN_URL/CLIENT_ID unset — no bearer");
                return "";
            }
            try {
                String form = "grant_type=client_credentials"
                        + "&client_id=" + URLEncoder.encode(clientId, StandardCharsets.UTF_8)
                        + "&client_secret=" + URLEncoder.encode(clientSecret, StandardCharsets.UTF_8)
                        + "&scope=" + URLEncoder.encode(scope, StandardCharsets.UTF_8);
                HttpRequest req = HttpRequest.newBuilder(URI.create(tokenUrl))
                        .timeout(Duration.ofSeconds(5))
                        .header("Content-Type", "application/x-www-form-urlencoded")
                        .POST(HttpRequest.BodyPublishers.ofString(form))
                        .build();
                HttpResponse<String> resp = http.send(req, HttpResponse.BodyHandlers.ofString());
                if (resp.statusCode() != 200) {
                    log.debugf("control-plane token fetch returned %d", resp.statusCode());
                    return "";
                }
                TokenResponse tr = JsonSerialization.readValue(resp.body(), TokenResponse.class);
                if (tr.accessToken == null || tr.accessToken.isEmpty()) {
                    return "";
                }
                cached = tr.accessToken;
                long ttl = tr.expiresIn > 30 ? tr.expiresIn - 30 : tr.expiresIn;
                expiresAt = Instant.now().plusSeconds(Math.max(ttl, 0));
                return cached;
            } catch (Exception e) {
                log.debugf(e, "control-plane token fetch failed");
                return "";
            }
        }
    }

    @JsonIgnoreProperties(ignoreUnknown = true)
    static final class TokenResponse {
        @JsonProperty("access_token")
        String accessToken;
        @JsonProperty("expires_in")
        long expiresIn;
    }
}
