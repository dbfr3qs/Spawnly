package run.spawnly.keycloak.ciba;

import org.jboss.logging.Logger;

import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ConcurrentMap;

/**
 * Shared, singleton mapping of a CIBA request's Keycloak {@code authResultId} to
 * the registry's consent-request id plus the spawn-edge context the completion
 * poller needs to resolve and record the grant. Ported from
 * identityserver/ConsentRequestTracker.cs.
 *
 * <p>WHY A SINGLETON: the channel provider (which records an entry when it
 * creates the registry consent request) and the completion poller (which
 * consumes it once the registry resolves) are separate provider instances
 * created per KeycloakSession. The tracker is held by the channel provider's
 * FACTORY (one per server) and shared with the poller, exactly like the C#
 * singleton read by both the transient notification service and the hosted
 * poller.
 *
 * <p>WHY IT ALSO CARRIES THE EDGE CONTEXT (unlike the C#, which re-reads it from
 * Duende's stored request Properties): Keycloak's device-code store does not
 * carry our edge properties, and the poller runs in a fresh KeycloakSession with
 * no request context. So the edge fields the poller needs to (a) approve the
 * device code by its {@code realmId + authResultId} and (b) record durable
 * consent (userId, parentType, childType, scopes, clientId, registry request id)
 * are captured here at track time.
 *
 * <p>IN-MEMORY + BOUNDED: entries are evicted on resolution and, defensively,
 * once past their CIBA expiry (a process restart between track and resolve loses
 * the mapping — the registry request stays resolvable via its own API/dashboard;
 * only the automatic device-code completion is lost, matching the C# best-effort
 * note). A hard cap prevents unbounded growth if the poller ever stalls.
 */
public class ConsentRequestTracker {

    private static final Logger log = Logger.getLogger(ConsentRequestTracker.class);

    /** Defensive cap on tracked entries (a healthy system holds only in-flight ones). */
    static final int MAX_ENTRIES = 10_000;

    /**
     * One tracked CIBA request awaiting a registry decision.
     *
     * @param realmId           realm the device code lives in (the poller sets the realm context)
     * @param authResultId      Keycloak device-code "user code" to approve/deny (== CIBAAuthenticationRequest.getAuthResultId())
     * @param registryRequestId the registry consent-request id to poll
     * @param userId            edge user (for recording durable consent — informational)
     * @param parentType        edge parent type
     * @param childType         edge child type
     * @param clientId          requesting client (for logging/audit)
     * @param scopes            requested scopes (approved verbatim to the registry)
     * @param expiresAtEpochMs  wall-clock expiry after which the entry is swept
     */
    public record Entry(
            String realmId,
            String authResultId,
            String registryRequestId,
            String userId,
            String parentType,
            String childType,
            String clientId,
            List<String> scopes,
            long expiresAtEpochMs) {
    }

    // Keyed by authResultId (globally unique — KeycloakModelUtils.generateId()).
    private final ConcurrentMap<String, Entry> entries = new ConcurrentHashMap<>();

    /** Records a tracked entry. No-op (with a warning) if the cap is hit. */
    public void track(Entry entry) {
        if (entries.size() >= MAX_ENTRIES) {
            log.warnf("consent-request tracker at cap (%d); dropping %s", MAX_ENTRIES, entry.authResultId());
            return;
        }
        entries.put(entry.authResultId(), entry);
    }

    /** Removes a tracked entry by authResultId. */
    public void untrack(String authResultId) {
        entries.remove(authResultId);
    }

    /** A point-in-time copy of the tracked entries (safe to iterate + mutate the map). */
    public List<Entry> snapshot() {
        return List.copyOf(entries.values());
    }

    /**
     * Sweeps entries whose CIBA expiry has passed, returning the count evicted.
     * Called on each poll tick so an approved-but-never-polled or abandoned
     * request cannot leak.
     */
    public int evictExpired(long nowEpochMs) {
        int[] removed = {0};
        entries.entrySet().removeIf(e -> {
            boolean expired = e.getValue().expiresAtEpochMs() > 0 && nowEpochMs > e.getValue().expiresAtEpochMs();
            if (expired) {
                removed[0]++;
            }
            return expired;
        });
        return removed[0];
    }

    /** Test/inspection helper. */
    Map<String, Entry> view() {
        return Map.copyOf(entries);
    }
}
