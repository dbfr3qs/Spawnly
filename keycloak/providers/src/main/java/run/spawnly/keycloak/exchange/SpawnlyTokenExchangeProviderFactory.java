package run.spawnly.keycloak.exchange;

import org.jboss.logging.Logger;
import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.protocol.oidc.TokenExchangeProvider;
import org.keycloak.protocol.oidc.TokenExchangeProviderFactory;

import run.spawnly.keycloak.attestor.AttestorFactory;
import run.spawnly.keycloak.attestor.OrgAwareVerifier;
import run.spawnly.keycloak.registry.AgentRegistryClient;

import java.util.LinkedHashMap;
import java.util.Map;

/**
 * Factory for {@link SpawnlyTokenExchangeProvider}, registered via
 * META-INF/services/org.keycloak.protocol.oidc.TokenExchangeProviderFactory.
 *
 * <p><b>Priority.</b> Keycloak's built-in {@code TokenExchangeGrantType} sorts
 * all token-exchange factories by {@link #order()} DESCENDING and dispatches to
 * the first provider whose {@code supports()} returns true. The built-in
 * Standard factory has {@code order()==10} and V1 has the default 0; this
 * factory returns {@code 100}, so it gets first refusal. Because
 * {@link SpawnlyTokenExchangeProvider#supports} returns true ONLY for a Spawnly
 * delegation exchange (actor_token present + self-issued subject_token), any
 * other exchange falls through to the built-in providers. So we OWN the Spawnly
 * delegation URN by priority WITHOUT dropping the {@code token-exchange} feature
 * flag (that flag only gates the legacy v1 grant; the standard endpoint is
 * default-on) — no collision.
 *
 * <p>The verifier (attestor seam) and the registry client are built ONCE at
 * factory init and shared across requests (both stateless/thread-safe), exactly
 * like the Phase-1 authenticator. An unknown {@code ATTESTOR} makes init throw —
 * a HARD FAIL at server startup, matching Program.cs.
 */
public class SpawnlyTokenExchangeProviderFactory implements TokenExchangeProviderFactory {

    public static final String PROVIDER_ID = "spawnly-token-exchange";
    private static final Logger log = Logger.getLogger(SpawnlyTokenExchangeProviderFactory.class);

    // Higher than the built-in Standard (10) / V1 (0) factories so this provider
    // is consulted first; it declines (supports()==false) for non-Spawnly
    // exchanges, letting the built-ins handle those.
    private static final int ORDER = 100;

    /**
     * scope → audience (resource) name. The SINGLE source of truth for this map
     * is deploy/keycloak/scopes.tf's {@code scope_audience} (itself ported from
     * Config.cs ApiResources). Kept in sync with it: on a normal
     * client_credentials mint Keycloak's per-scope audience mappers set {@code
     * aud}; on the token-exchange path this provider mints the token directly
     * (bypassing the mapper pipeline, which needs a real user session), so it
     * must reproduce the same scope→aud mapping here.
     */
    private static final Map<String, String> SCOPE_AUDIENCE;

    static {
        Map<String, String> m = new LinkedHashMap<>();
        m.put("sample-api-a:read", "sample-api-a");
        m.put("sample-api-a:write", "sample-api-a");
        m.put("sample-api-b:read", "sample-api-b");
        m.put("sample-api-b:write", "sample-api-b");
        m.put("fx:read", "travel-tools");
        m.put("flights:read", "travel-tools");
        m.put("hotels:read", "travel-tools");
        m.put("registry.consent", "registry");
        m.put("orchestrator:spawn", "orchestrator");
        m.put("orchestrator:read", "orchestrator");
        m.put("orchestrator:write", "orchestrator");
        SCOPE_AUDIENCE = Map.copyOf(m);
    }

    private volatile OrgAwareVerifier verifier;
    private volatile AgentRegistryClient registry;

    @Override
    public void init(Config.Scope config) {
        // Select the attestor once (unknown value ⇒ IllegalStateException ⇒
        // startup fails loudly). Same seam as the Phase-1 authenticator. Wrap it in
        // the Phase-9 org-aware resolver (STS_TRUSTED_ENDPOINT honoured) so a remote
        // (STS) agent's actor_token is verified against its org anchor when it
        // delegates.
        this.registry = new AgentRegistryClient();
        this.verifier = new OrgAwareVerifier(
                AttestorFactory.build(config), registry, AttestorFactory.resolveStsTrustedEndpoint(config));
        log.debug("spawnly-token-exchange provider initialized");
    }

    @Override
    public TokenExchangeProvider create(KeycloakSession session) {
        return new SpawnlyTokenExchangeProvider(session, verifier, registry, SCOPE_AUDIENCE);
    }

    @Override
    public void postInit(KeycloakSessionFactory factory) {
    }

    @Override
    public void close() {
    }

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public int order() {
        return ORDER;
    }
}
