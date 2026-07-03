package run.spawnly.keycloak.ciba;

import org.jboss.logging.Logger;
import org.keycloak.Config;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.KeycloakSessionFactory;
import org.keycloak.models.utils.KeycloakModelUtils;
import org.keycloak.protocol.oidc.grants.ciba.channel.AuthenticationChannelProvider;
import org.keycloak.protocol.oidc.grants.ciba.channel.AuthenticationChannelProviderFactory;
import org.keycloak.provider.EnvironmentDependentProviderFactory;
import org.keycloak.timer.TimerProvider;

import run.spawnly.keycloak.registry.AgentRegistryClient;

/**
 * Factory for {@link SpawnlyAuthenticationChannelProvider}, registered via
 * META-INF/services/org.keycloak.protocol.oidc.grants.ciba.channel.AuthenticationChannelProviderFactory.
 *
 * <p><b>How it takes over CIBA.</b> Keycloak's CIBA backchannel endpoint resolves
 * a single {@code AuthenticationChannelProvider} from the realm. The built-in one
 * is the HTTP decoupled-channel provider ({@code
 * HttpAuthenticationChannelProvider}); by registering our factory as the default
 * ({@code isDefault()}), the realm uses THIS provider, which drives consent
 * through the REGISTRY (dashboard/mobile) instead of an external HTTP channel or
 * Keycloak's built-in consent screen. So the spawn edge is answered exactly the
 * way IdentityServer did.
 *
 * <p><b>Shared singletons + the completion poller.</b> The registry client and
 * the {@link ConsentRequestTracker} are built ONCE at factory init and shared
 * across the per-request provider instances (the HTTP client + tracker are
 * thread-safe). In {@code postInit} the factory schedules the
 * {@link SpawnlyConsentCompletionPoller} on Keycloak's {@link TimerProvider} — a
 * single background component (the {@code ConsentCompletionPoller} role) that
 * resolves tracked pending requests against the registry.
 *
 * <p>{@code isSupported}: CIBA is a supported (default-on) feature on the pinned
 * Keycloak 26.4, so this factory is always available; the realm still gates
 * whether the CIBA grant is enabled per-client.
 */
public class SpawnlyAuthenticationChannelProviderFactory
        implements AuthenticationChannelProviderFactory, EnvironmentDependentProviderFactory {

    public static final String PROVIDER_ID = "spawnly-ciba-channel";
    private static final Logger log = Logger.getLogger(SpawnlyAuthenticationChannelProviderFactory.class);

    // Poll the registry for pending decisions every 2s, matching the C#
    // ConsentCompletionPoller's 2s interval. Fast enough that the sidecar's
    // 5s token-poll interval sees a resolved decision promptly.
    static final long POLL_INTERVAL_MS = 2_000L;

    private volatile AgentRegistryClient registry;
    private volatile ConsentRequestTracker tracker;
    private volatile boolean devCibaApi;

    @Override
    public void init(Config.Scope config) {
        this.registry = new AgentRegistryClient();
        this.tracker = new ConsentRequestTracker();
        this.devCibaApi = "true".equals(System.getenv("DEV_CIBA_API"));
        log.debug("spawnly-ciba-channel provider initialized");
    }

    @Override
    public void postInit(KeycloakSessionFactory factory) {
        // Start the completion poller on the timer. Scheduling itself runs in a
        // short transaction; the task then runs periodically in its own sessions.
        final AgentRegistryClient reg = this.registry;
        final ConsentRequestTracker trk = this.tracker;
        KeycloakModelUtils.runJobInTransaction(factory, session -> {
            TimerProvider timer = session.getProvider(TimerProvider.class);
            if (timer == null) {
                log.error("TimerProvider unavailable; CIBA consent completion poller NOT started");
                return;
            }
            timer.scheduleTask(new SpawnlyConsentCompletionPoller(reg, trk),
                    POLL_INTERVAL_MS, SpawnlyConsentCompletionPoller.TASK_NAME);
            log.infof("scheduled CIBA consent completion poller (every %dms)", POLL_INTERVAL_MS);
        });
    }

    @Override
    public AuthenticationChannelProvider create(KeycloakSession session) {
        return new SpawnlyAuthenticationChannelProvider(session, registry, tracker, devCibaApi);
    }

    @Override
    public void close() {
    }

    @Override
    public String getId() {
        return PROVIDER_ID;
    }

    @Override
    public boolean isSupported(Config.Scope config) {
        // CIBA is a supported feature on this Keycloak; always available.
        return true;
    }
}
