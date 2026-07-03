package run.spawnly.keycloak.ciba;

import org.jboss.logging.Logger;
import org.keycloak.models.KeycloakSession;
import org.keycloak.models.RealmModel;
import org.keycloak.protocol.oidc.grants.device.DeviceGrantType;
import org.keycloak.timer.ScheduledTask;

import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.ConsentRequest;

import java.util.List;

/**
 * Bridges the registry-owned consent decision back into Keycloak's CIBA flow.
 * Ported from identityserver/ConsentCompletionPoller.cs + CibaCompletionService.cs.
 *
 * <p>The registry (not the IdP) owns the pending→approved/denied lifecycle: when
 * a user approves/denies in the dashboard the decision lands in the registry.
 * But the waiting agent is polling Keycloak's token endpoint, which only mints
 * once the device code behind the {@code auth_req_id} is APPROVED. Nothing in the
 * approve-at-the-registry path flips it — this scheduled task closes that gap
 * while respecting the dependency direction (the IdP polls the registry; the
 * registry never calls the IdP).
 *
 * <p>Runs on Keycloak's {@code TimerProvider} every few seconds. On each tick it
 * snapshots the {@link ConsentRequestTracker}, and for each tracked pending
 * request polls the registry consent-request status and, when resolved:
 * <ul>
 *   <li><b>approved</b> → {@link DeviceGrantType#approveUserCode} the device code
 *       (the same call Keycloak's built-in CIBA callback endpoint makes), then
 *       records durable consent in the registry (idempotent — the registry
 *       already holds the grant when it auto-approved) and untracks;</li>
 *   <li><b>denied</b> → {@link DeviceGrantType#denyUserCode} (the token poll then
 *       returns {@code access_denied}) and untracks;</li>
 *   <li><b>pending</b> → leave it for the next tick.</li>
 * </ul>
 * A covering STORED CONSENT makes the registry report {@code approved} on the
 * first poll, so the request is approved on the first tick — the observable
 * "auto-approve" behaviour of the C# inline path.
 *
 * <p>Expiry + eviction: each tick first sweeps entries past their CIBA expiry
 * (the device code has expired in Keycloak's store anyway; the token poll returns
 * {@code expired_token}). A device code that vanished from the store (expired /
 * already redeemed) is treated as resolved and untracked, so the tracker stays
 * bounded.
 */
public class SpawnlyConsentCompletionPoller implements ScheduledTask {

    public static final String TASK_NAME = "spawnly-ciba-consent-poller";
    private static final Logger log = Logger.getLogger(SpawnlyConsentCompletionPoller.class);

    private final AgentRegistryClient registry;
    private final ConsentRequestTracker tracker;

    public SpawnlyConsentCompletionPoller(AgentRegistryClient registry, ConsentRequestTracker tracker) {
        this.registry = registry;
        this.tracker = tracker;
    }

    @Override
    public String getTaskName() {
        return TASK_NAME;
    }

    @Override
    public void run(KeycloakSession session) {
        int evicted = tracker.evictExpired(System.currentTimeMillis());
        if (evicted > 0) {
            log.debugf("swept %d expired CIBA consent entries", evicted);
        }

        for (ConsentRequestTracker.Entry entry : tracker.snapshot()) {
            try {
                pollOne(session, entry);
            } catch (RuntimeException e) {
                // Never let one entry's failure stall the others; retry next tick.
                log.debugf(e, "CIBA consent poll failed for %s", entry.authResultId());
            }
        }
    }

    private void pollOne(KeycloakSession session, ConsentRequestTracker.Entry entry) {
        ConsentRequest cr = registry.getConsentRequest(entry.registryRequestId());
        if (cr == null) {
            // Registry unreachable / transient error ⇒ retry next tick (fail closed:
            // no completion happens until the registry answers).
            return;
        }
        if (cr.isPending()) {
            return;
        }

        RealmModel realm = session.realms().getRealm(entry.realmId());
        if (realm == null) {
            // Realm gone (unlikely) ⇒ stop tracking.
            tracker.untrack(entry.authResultId());
            return;
        }
        session.getContext().setRealm(realm);

        if (cr.isApproved()) {
            boolean ok = DeviceGrantType.approveUserCode(session, realm, entry.authResultId(), "spawnly-ciba", null);
            if (ok) {
                // Record durable consent so the NEXT spawn of this edge auto-approves.
                // Idempotent: on a stored-consent auto-approve the registry already
                // holds the grant; approving again is a no-op / refresh.
                recordDurableConsent(entry);
                log.infof("CIBA completed from registry approval: %s %s->%s (agent client %s)",
                        entry.userId(), entry.parentType(), entry.childType(), entry.clientId());
            } else {
                // The device code has already expired / been redeemed. Nothing left
                // to complete; drop it.
                log.debugf("CIBA approve: device code %s no longer pending", entry.authResultId());
            }
            tracker.untrack(entry.authResultId());
        } else if (cr.isDenied()) {
            DeviceGrantType.denyUserCode(session, realm, entry.authResultId());
            log.infof("CIBA failed from registry denial: %s %s->%s",
                    entry.userId(), entry.parentType(), entry.childType());
            tracker.untrack(entry.authResultId());
        } else {
            // Unknown terminal status — untrack defensively so it can't leak.
            log.warnf("CIBA consent request %s has unexpected status '%s'; untracking",
                    entry.registryRequestId(), cr.status());
            tracker.untrack(entry.authResultId());
        }
    }

    /**
     * Records the durable consent for the edge on approval. The registry derives
     * the expiry from the parent template's consentTTL and replaces any prior
     * record for the edge (mirrors the C# ApproveConsentRequest → ConsentRecord).
     */
    private void recordDurableConsent(ConsentRequestTracker.Entry entry) {
        List<String> scopes = entry.scopes() == null ? List.of() : entry.scopes();
        if (registry.approveConsentRequest(entry.registryRequestId(), scopes) == null) {
            // The device code is already approved (the agent will get its token);
            // only the durable record failed, so the NEXT spawn re-prompts instead
            // of auto-approving. Best-effort, matching the C# warning path.
            log.warnf("consent approved but registry consent request %s not recorded for %s %s->%s",
                    entry.registryRequestId(), entry.userId(), entry.parentType(), entry.childType());
        }
    }
}
