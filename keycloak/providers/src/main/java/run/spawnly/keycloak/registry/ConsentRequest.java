package run.spawnly.keycloak.registry;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;

/**
 * A registry consent request, ported from the {@code ConsentRequest} record in
 * identityserver/AgentRegistryClient.cs. This is the registry-owned object that
 * models one pending "may agent X act for user U on the parentType→childType
 * spawn edge?" decision — created/reused at CIBA backchannel time and resolved
 * (approved/denied) by the dashboard/mobile user.
 *
 * <p>Registry JSON is camelCase:
 * {@code id, userId, parentType, childType, agentId, scopes, bindingMessage,
 * status, createdAt, resolvedAt, externalRef}. Unknown fields are ignored so the
 * registry can evolve its schema without breaking the IdP (forward-compatible,
 * dependency-direction-safe).
 *
 * <p>{@code status} is one of {@code pending | approved | denied} (mirroring the
 * C#). The Phase 4 CIBA pipeline treats {@code approved} as "auto-approve /
 * complete", {@code denied} as "fail", and {@code pending} as "keep polling".
 *
 * @param id             registry primary key of the consent request
 * @param userId         the human who must approve (the agent's userId)
 * @param parentType     the parent agent's type on the spawn edge
 * @param childType      the child (requesting) agent's type on the spawn edge
 * @param agentId        the requesting agent's id (for the dashboard's context)
 * @param scopes         the scopes the child is asking to be granted
 * @param bindingMessage the human-readable binding message shown to the approver
 * @param status         {@code pending | approved | denied}
 * @param externalRef    the CIBA request's internal id (for correlation)
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record ConsentRequest(
        @JsonProperty("id") String id,
        @JsonProperty("userId") String userId,
        @JsonProperty("parentType") String parentType,
        @JsonProperty("childType") String childType,
        @JsonProperty("agentId") String agentId,
        @JsonProperty("scopes") List<String> scopes,
        @JsonProperty("bindingMessage") String bindingMessage,
        @JsonProperty("status") String status,
        @JsonProperty("externalRef") String externalRef) {

    public static final String STATUS_PENDING = "pending";
    public static final String STATUS_APPROVED = "approved";
    public static final String STATUS_DENIED = "denied";

    public boolean isPending() {
        return STATUS_PENDING.equals(status);
    }

    public boolean isApproved() {
        return STATUS_APPROVED.equals(status);
    }

    public boolean isDenied() {
        return STATUS_DENIED.equals(status);
    }
}
