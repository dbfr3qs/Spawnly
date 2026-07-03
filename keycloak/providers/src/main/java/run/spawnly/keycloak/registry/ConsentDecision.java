package run.spawnly.keycloak.registry;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * A registry consent-coverage decision, ported from the {@code ConsentDecision}
 * in identityserver/AgentRegistryClient.cs. Consumed by the Phase 4 CIBA
 * auto-approve check.
 *
 * @param granted whether a stored consent currently covers the requested edge + scopes
 * @param reason  optional human-readable explanation (for logs)
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record ConsentDecision(
        @JsonProperty("granted") boolean granted,
        @JsonProperty("reason") String reason) {
}
