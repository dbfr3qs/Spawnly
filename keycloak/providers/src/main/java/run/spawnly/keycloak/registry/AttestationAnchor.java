package run.spawnly.keycloak.registry;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * An org's per-org trust anchor, mirroring Go
 * {@code registry.AttestationAnchor}. First (and only current) mode is STS trust
 * federation with the consumer's AWS account: an agent recorded under this org
 * authenticates by presenting an assumed-role STS credential whose account
 * equals {@link #awsAccountId()} and whose ARN matches {@link #roleArnPattern()}.
 *
 * <p>The anchor registers {@code pending} and flips to {@code active} only after
 * the org proves control of the account (Phase 8). Per-org verifier resolution
 * (Phase 9) selects an org-bound verifier ONLY when the anchor is {@code active}
 * and of type {@code sts}; a pending/absent anchor leaves the org unanchored (its
 * STS agents cannot authenticate — the isolation invariant).
 *
 * <p>Unknown fields are ignored so the registry can extend the record without
 * breaking the IdP (forward-compatible, dependency-direction-safe).
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record AttestationAnchor(
        @JsonProperty("type") String type,
        @JsonProperty("awsAccountId") String awsAccountId,
        @JsonProperty("roleArnPattern") String roleArnPattern,
        @JsonProperty("status") String status) {

    public static final String STATUS_ACTIVE = "active";
    public static final String TYPE_STS = "sts";

    /**
     * True only for an anchor that is both {@code active} and of type {@code sts}
     * — the sole condition under which per-org STS verifier resolution applies.
     * A pending or non-STS anchor is treated as unanchored (fail-closed).
     */
    public boolean isActiveSts() {
        return STATUS_ACTIVE.equals(status) && TYPE_STS.equals(type);
    }
}
