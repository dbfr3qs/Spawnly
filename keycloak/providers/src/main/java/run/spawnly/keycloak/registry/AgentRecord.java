package run.spawnly.keycloak.registry;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * The registry's agent record, ported from the {@code AgentRecord} in
 * identityserver/AgentRegistryClient.cs. Registry JSON is camelCase:
 * {@code agentId, status, userId, agentType, parentId}.
 *
 * <p>Unknown fields are ignored so the registry can add fields without breaking
 * the IdP (forward-compatible, dependency-direction-safe).
 *
 * @param agentId   registry primary key (the SVID's last path segment)
 * @param status    lifecycle status; {@code active} is required for the
 *                  client_credentials liveness gate
 * @param userId    the human the agent acts for → {@code sub=user:<userId>}
 * @param agentType the agent's type, which the platform models as the Keycloak
 *                  {@code client_id}; bound against the authenticating client
 * @param parentId  parent agent id (used by delegation in Phase 3)
 * @param orgId     the organization the agent belongs to (multi-tenancy), or
 *                  {@code null}/empty for an org-less (global) agent. Recorded at
 *                  spawn and derived SERVER-SIDE by the registry — the IdP treats
 *                  it as the authoritative, registry-bound source of the token's
 *                  {@code organization} claim (never copied from a request token).
 * @param placement WHERE the agent runs (remote-substrate): empty/{@code "k8s"} =
 *                  platform-hosted (the default), {@code "remote"} = #47 STS agent,
 *                  {@code "substrate"} = consumer-hosted substrate. Server-set at
 *                  preregistration and IMMUTABLE (a self-registering sidecar never
 *                  sets it from its body). The mint-side reverse org-binding refuses
 *                  a platform (org-less) SVID against a record ONLY when placement is
 *                  {@code "substrate"} — a substrate record must be claimed solely by
 *                  its org-scoped substrate SVID, while an org member's PLATFORM
 *                  agent (org-less SVID + org-scoped record) stays valid (spec AC 9).
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record AgentRecord(
        @JsonProperty("agentId") String agentId,
        @JsonProperty("status") String status,
        @JsonProperty("userId") String userId,
        @JsonProperty("agentType") String agentType,
        @JsonProperty("parentId") String parentId,
        @JsonProperty("orgId") String orgId,
        @JsonProperty("placement") String placement) {

    /** Placement value marking a consumer-hosted substrate agent (mirrors the Go
     *  {@code registry.PlacementSubstrate}). */
    public static final String PLACEMENT_SUBSTRATE = "substrate";

    /**
     * Backward-compatible constructor for the org-less case (and the many call
     * sites that predate multi-tenancy): delegates to the canonical constructor
     * with {@code orgId=null}. Jackson always uses the canonical (annotated)
     * constructor, so this only affects hand-built records.
     */
    public AgentRecord(String agentId, String status, String userId, String agentType, String parentId) {
        this(agentId, status, userId, agentType, parentId, null, null);
    }

    /**
     * Backward-compatible constructor for call sites that set an org but predate
     * remote-substrate placement: delegates with {@code placement=null} (== the
     * platform-hosted default).
     */
    public AgentRecord(String agentId, String status, String userId, String agentType, String parentId, String orgId) {
        this(agentId, status, userId, agentType, parentId, orgId, null);
    }
}
