package run.spawnly.keycloak.registry;

import com.fasterxml.jackson.annotation.JsonIgnoreProperties;
import com.fasterxml.jackson.annotation.JsonProperty;

/**
 * The registry's organization record, mirroring Go {@code registry.Organization}
 * — the SaaS business account (distinct from the per-consumer tenant primitive).
 * The IdP reads it (via the control-plane-gated {@code GET /v1/orgs/{id}}) ONLY
 * to resolve an org's optional attestation {@link #anchor()} during per-org
 * verifier resolution (Phase 9). Membership is NOT here — Keycloak owns who's in
 * an org, SpiceDB owns authorization.
 *
 * <p>Unknown fields are ignored so the registry can extend the record without
 * breaking the IdP (forward-compatible, dependency-direction-safe).
 */
@JsonIgnoreProperties(ignoreUnknown = true)
public record Organization(
        @JsonProperty("orgId") String orgId,
        @JsonProperty("name") String name,
        @JsonProperty("anchor") AttestationAnchor anchor) {
}
