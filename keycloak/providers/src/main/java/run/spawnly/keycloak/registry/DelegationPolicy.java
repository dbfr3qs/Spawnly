package run.spawnly.keycloak.registry;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

import java.util.List;

/**
 * Delegation policy for a (parentType → childType) edge, ported from the
 * {@code DelegationPolicy} in identityserver/AgentRegistryClient.cs. Consumed by
 * the Phase 3 token-exchange extension.
 *
 * <p>GET /v1/delegation-policy?parentType=X&amp;childType=Y →
 * {@code { "allowed": bool, "grantableScopes": ["..."], "maxDepth": int }}.
 *
 * @param allowed         whether the edge is permitted at all
 * @param grantableScopes the ceiling of scopes the parent may delegate on this edge
 * @param maxDepth        the maximum actor-chain depth this edge permits
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record DelegationPolicy(
        @JsonProperty("allowed") boolean allowed,
        @JsonProperty("grantableScopes") List<String> grantableScopes,
        @JsonProperty("maxDepth") int maxDepth) {
}
