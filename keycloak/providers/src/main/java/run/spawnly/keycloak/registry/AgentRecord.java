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
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record AgentRecord(
        @JsonProperty("agentId") String agentId,
        @JsonProperty("status") String status,
        @JsonProperty("userId") String userId,
        @JsonProperty("agentType") String agentType,
        @JsonProperty("parentId") String parentId) {
}
