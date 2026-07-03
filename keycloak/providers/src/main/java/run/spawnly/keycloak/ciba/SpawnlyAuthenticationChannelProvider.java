package run.spawnly.keycloak.ciba;

import org.jboss.logging.Logger;
import org.keycloak.models.KeycloakSession;
import org.keycloak.protocol.oidc.grants.ciba.channel.AuthenticationChannelProvider;
import org.keycloak.protocol.oidc.grants.ciba.channel.CIBAAuthenticationRequest;

import run.spawnly.keycloak.identity.VerifiedAgent;
import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.ConsentRequest;

import java.util.Arrays;
import java.util.List;

/**
 * The Spawnly CIBA authentication-channel provider — the Keycloak port of the
 * C# CIBA request/consent pipeline (CibaRequestValidator + CibaUserValidator +
 * CibaConsentNotificationService).
 *
 * <p><b>Where it runs.</b> Keycloak's {@code BackchannelAuthenticationEndpoint}
 * first authenticates the calling client (our {@code spawnly-attestation}
 * authenticator verifies the SVID + runs the grant-aware liveness gate, and
 * stashes the registry-confirmed {@link VerifiedAgent} on the KeycloakSession),
 * resolves the {@code login_hint} to a Keycloak user, then calls
 * {@link #requestAuthentication}. Returning {@code true} makes Keycloak persist
 * the pending device code and answer the sidecar with an {@code auth_req_id};
 * returning {@code false} fails the backchannel request.
 *
 * <p><b>What it does</b> (semantics identical to the C#):
 * <ol>
 *   <li><b>Bind the spawn edge from the VERIFIED SVID, never from params.</b> The
 *       {@link VerifiedAgent} (agentId + userId) comes from the attestation
 *       authenticator's registry-confirmed identity — NOT from any client-supplied
 *       hint. From the agent record we take childType (agentType) and, via its
 *       parentId, the parentType. This is the security core: a caller cannot
 *       assert an edge it does not own.</li>
 *   <li><b>login_hint cross-check.</b> The resolved user (its username, passed as
 *       {@code infoUsedByAuthentication}) MUST equal the agent's userId. So the
 *       login_hint cannot select an approver other than the agent's real user.</li>
 *   <li><b>Parentless / terminal agents are rejected</b> — consent gates a
 *       parent→child spawn edge; a root spawn is the user's own action.</li>
 *   <li><b>Create-or-reuse the registry consent request</b> for
 *       (userId, parentType, childType, scopes). If it is already
 *       {@code approved} (a stored consent covers the scopes) it is tracked for
 *       AUTO-APPROVE; otherwise it stays pending. Either way the request is
 *       TRACKED (authResultId ↔ registry request id + edge) so the completion
 *       poller can resolve it, and {@code true} is returned so the device code is
 *       stored.</li>
 * </ol>
 *
 * <p><b>Why completion is deferred to the poller (not done inline here).</b> The
 * device code does not exist yet when {@code requestAuthentication} runs —
 * Keycloak stores it only AFTER we return {@code true}. So even an auto-approve
 * cannot flip the (nonexistent) device code here; instead we track it and the
 * {@link SpawnlyConsentCompletionPoller} approves it on its next tick (for a
 * covering stored consent the registry already reports {@code approved}, so the
 * first poll approves it and the sidecar's first token poll succeeds — the same
 * observable behaviour as the C# inline auto-approve).
 *
 * <p>DEPENDENCY DIRECTION: depends only on the registry's neutral HTTP contract
 * ({@link AgentRegistryClient}) + Keycloak's CIBA SPI — never the reverse.
 * Keycloak's built-in consent screen is NOT used; consent is registry-native.
 */
public class SpawnlyAuthenticationChannelProvider implements AuthenticationChannelProvider {

    private static final Logger log = Logger.getLogger(SpawnlyAuthenticationChannelProvider.class);

    private final KeycloakSession session;
    private final AgentRegistryClient registry;
    private final ConsentRequestTracker tracker;
    private final boolean devCibaApi;

    public SpawnlyAuthenticationChannelProvider(KeycloakSession session, AgentRegistryClient registry,
                                                ConsentRequestTracker tracker, boolean devCibaApi) {
        this.session = session;
        this.registry = registry;
        this.tracker = tracker;
        this.devCibaApi = devCibaApi;
    }

    @Override
    public boolean requestAuthentication(CIBAAuthenticationRequest request, String infoUsedByAuthentication) {
        // The user the request asks to authenticate (its username; sub=username
        // for humans). Used ONLY to cross-check the agent's real user, never to
        // select an approver.
        String hintedUser = infoUsedByAuthentication;

        // ── Bind the edge from the VERIFIED SVID (via the attestation authenticator) ──
        VerifiedAgent verified = session.getAttribute(VerifiedAgent.SESSION_ATTR, VerifiedAgent.class);
        if (verified == null) {
            // No attestation-authenticated identity on this session. Non-SVID
            // client auth only exists off-cluster (local dev drives CIBA with a
            // placeholder client secret), and there is no agent record to resolve
            // the edge — so this is rejected unless the dev API is explicitly on.
            if (!devCibaApi) {
                log.warn("CIBA backchannel request rejected: client_assertion required for CIBA");
                return false;
            }
            // DEV_CIBA_API fallback: no real edge; track nothing, leave pending
            // for a manual/dashboard resolution (parity with the C# dev branch).
            log.warn("CIBA DEV_CIBA_API: accepting request with no verified edge (dev only)");
            return true;
        }

        String agentId = verified.agentId();
        String userId = verified.userId();

        AgentRecord agent = registry.getAgent(agentId);
        if (agent == null) {
            log.warnf("CIBA backchannel request rejected: agent %s not registered", agentId);
            return false;
        }
        // Status was already checked (active|pending|awaiting-consent) by the
        // grant-aware liveness gate at client auth; re-derive the edge fields.
        if (agent.userId() == null || agent.userId().isEmpty()) {
            log.warnf("CIBA backchannel request rejected: agent %s has no user to ask for consent", agentId);
            return false;
        }
        // login_hint must match the agent's real user — the login_hint cannot
        // select an arbitrary approver beyond the agent's own userId.
        if (hintedUser == null || !hintedUser.equals(agent.userId())) {
            log.warnf("CIBA backchannel request rejected: login_hint '%s' does not match agent %s user '%s'",
                    hintedUser, agentId, agent.userId());
            return false;
        }
        if (agent.parentId() == null || agent.parentId().isEmpty()) {
            // A parentless agent has no spawn edge to consent to (root spawns are
            // the user's own action).
            log.warnf("CIBA backchannel request rejected: agent %s has no parent; consent applies to spawned children",
                    agentId);
            return false;
        }
        AgentRecord parent = registry.getAgent(agent.parentId());
        if (parent == null || parent.agentType() == null || parent.agentType().isEmpty()) {
            log.warnf("CIBA backchannel request rejected: parent %s of agent %s is unknown",
                    agent.parentId(), agentId);
            return false;
        }

        String parentType = parent.agentType();
        String childType = (agent.agentType() != null && !agent.agentType().isEmpty())
                ? agent.agentType() : request.getClient().getClientId();
        // The consent user is the AGENT's registry userId (authoritative), which
        // we have already confirmed equals the login_hint above.
        String consentUser = agent.userId();

        List<String> scopes = splitScopes(request.getScope());
        String authResultId = request.getAuthResultId();
        String bindingMessage = request.getBindingMessage();

        // ── Create-or-reuse the registry consent request ──────────────────────
        ConsentRequest cr = registry.createConsentRequest(
                consentUser, parentType, childType, scopes, bindingMessage,
                /* externalRef */ authResultId, /* agentId */ agentId);

        if (cr == null) {
            // Registry unreachable: leave the request PENDING (matching the C#,
            // which logs "registry unreachable" and returns without tracking).
            // Nothing to track ⇒ the poller can't auto-complete it; a later
            // dashboard action against the registry request (once the registry is
            // back) still resolves it via the registry's own flow. We still
            // return true so the sidecar gets an auth_req_id and keeps polling.
            log.warnf("CIBA pending (registry unreachable): %s %s->%s", consentUser, parentType, childType);
            return true;
        }

        long cibaExpiryMs = request.getExp() > 0 ? request.getExp() * 1000L : 0L;
        tracker.track(new ConsentRequestTracker.Entry(
                session.getContext().getRealm().getId(),
                authResultId,
                cr.id(),
                consentUser,
                parentType,
                childType,
                request.getClient().getClientId(),
                scopes,
                cibaExpiryMs));

        if (cr.isApproved()) {
            log.infof("CIBA auto-approve tracked from stored consent: %s %s->%s", consentUser, parentType, childType);
        } else {
            log.infof("CIBA pending user approval: %s %s->%s", consentUser, parentType, childType);
        }
        return true;
    }

    @Override
    public void close() {
        // Registry client + tracker are factory-owned and shared; nothing to release.
    }

    static List<String> splitScopes(String scope) {
        if (scope == null || scope.isBlank()) {
            return List.of();
        }
        return Arrays.stream(scope.trim().split("\\s+"))
                .filter(s -> !s.isEmpty())
                .distinct()
                .toList();
    }
}
