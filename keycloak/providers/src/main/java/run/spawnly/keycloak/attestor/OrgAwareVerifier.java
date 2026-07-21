package run.spawnly.keycloak.attestor;

import org.jboss.logging.Logger;

import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.AttestationAnchor;
import run.spawnly.keycloak.registry.Organization;

/**
 * Per-org attestation-credential resolution with a strict isolation invariant —
 * the shared core of Phase 9, used by BOTH the client authenticator (agent
 * client_credentials mint) and the token-exchange provider (delegation
 * actor_token), so remote (consumer-hosted) agents can authenticate AND delegate.
 *
 * <h2>The isolation invariant (spec criterion 12)</h2>
 * An STS credential verifies ONLY against the anchor of the org that owns the
 * agent's registry record. The AWS-attested account MUST equal that org's active
 * anchor account and the assumed-role ARN MUST match the anchor's role pattern.
 * Consequences, all enforced here:
 * <ul>
 *   <li>org A's AWS account can never authenticate an agent recorded under org B
 *       (account binding);</li>
 *   <li>an unanchored (or {@code pending}-anchor) org's agents can never ride any
 *       anchor (no active anchor ⇒ refuse);</li>
 *   <li>there is NO fallback across verifiers — a failed org-STS resolution is a
 *       refusal, never a retry against the platform default;</li>
 *   <li>SPIRE (platform) agents are UNAFFECTED by any org's anchor — a non-STS
 *       credential is routed straight to the platform default verifier, so the
 *       SPIRE path is bit-identical to before.</li>
 * </ul>
 *
 * <h2>Oracle-free</h2>
 * Every failure — bad credential, unknown record, wrong org, unanchored org,
 * registry/getOrg outage — returns {@code null}, which both callers render as the
 * same {@code invalid_client}. Nothing distinguishes the failure modes to the
 * caller.
 *
 * <h2>Peek → record → verify → bind, for STS</h2>
 * The plan's peek-before-verify inverts the old "verify-then-look-up" order so
 * the RIGHT verifier is chosen before trusting anything. For an STS credential
 * the claimed agentId is the assumed-role session name, which lives ONLY in the
 * STS response — never in the presigned request — so the STS replay IS the peek:
 * it yields the AWS-attested {@code (account, arn, sessionName=agentId)} in one
 * round-trip. We then look up the record for that agentId and BIND the attested
 * account+arn to THAT record's org anchor. The agentId is AWS-attested (not a
 * caller claim), so binding it to the record's anchor is exactly the isolation
 * check; the org anchor account/role are the untrusted-until-matched gate.
 * (Deviation from the literal plan text, which imagined a peekable session name;
 * the single-replay realization is equivalent and avoids a wasteful double
 * replay on the token endpoint's hottest path.)
 *
 * <p>Verifier selection is keyed on the credential SHAPE (STS presigned envelope
 * vs. a JWT-SVID), NOT on the record's org: a SPIRE agent that happens to belong
 * to an STS-anchored org must still authenticate via SPIRE, which the
 * matrix/spec require. A shape misclassification can only ever cause a refusal,
 * never a cross-verifier bypass.
 *
 * <p>DEPENDENCY DIRECTION: depends only on the neutral registry HTTP client and
 * the attestation credential format + STS helpers — never on Keycloak internals.
 */
public final class OrgAwareVerifier {

    private static final Logger log = Logger.getLogger(OrgAwareVerifier.class);

    private final AgentCredentialVerifier defaultVerifier;
    private final AgentCredentialVerifier stsVerifier;
    private final AgentRegistryClient registry;

    /**
     * Production ctor: the platform default verifier (from {@link AttestorFactory})
     * plus an STS replay verifier built with the {@code STS_TRUSTED_ENDPOINT}
     * override so it targets the in-cluster fake STS in e2e. The STS verifier is
     * always available even when the default is SPIRE — that is what lets a remote
     * STS agent authenticate against a SPIRE-default platform.
     */
    public OrgAwareVerifier(AgentCredentialVerifier defaultVerifier, AgentRegistryClient registry,
                            String stsTrustedEndpoint) {
        this(defaultVerifier, new StsCredentialVerifier(stsTrustedEndpoint), registry);
    }

    /**
     * Composition seam: inject both the default and the STS replay verifier
     * directly (used by tests to drive the org-binding logic with a stubbed STS
     * replay). Production code uses the {@code (default, registry, endpoint)} ctor.
     */
    public OrgAwareVerifier(AgentCredentialVerifier defaultVerifier, AgentCredentialVerifier stsVerifier,
                            AgentRegistryClient registry) {
        this.defaultVerifier = defaultVerifier;
        this.stsVerifier = stsVerifier;
        this.registry = registry;
    }

    /**
     * Verifies {@code credential}, resolving the org-bound verifier for an STS
     * presentation and the platform default for everything else. Returns the
     * verified, org-bound {@link AgentIdentity}, or {@code null} on ANY failure
     * (oracle-free). Never throws for a bad credential.
     */
    public AgentIdentity verify(String credential) {
        if (looksLikeStsCredential(credential)) {
            return verifyStsOrgBound(credential);
        }
        // Non-STS (JWT-SVID / SPIRE) → the platform default verifier, then the
        // SPIRE org-bound binding (remote-substrate Phase 1) beside the STS one.
        return verifySpireOrgBound(credential);
    }

    /**
     * The SPIRE org-bound path (remote-substrate Phase 1), the mirror of
     * {@link #verifyStsOrgBound} with the SAME fail-closed discipline. The
     * default verifier (SpireCredentialVerifier, in trust-domain-enforcing mode)
     * surfaces the signed-path org on {@link AgentIdentity#orgId()}:
     * <ul>
     *   <li>org-LESS identity (orgId null/empty) ⇒ platform-hosted (or a
     *       non-SPIFFE default that never sets orgId) ⇒ passthrough,
     *       bit-identical, the org store is never touched;</li>
     *   <li>org-SCOPED identity ⇒ authenticates ONLY against a record whose org
     *       equals the SVID's org. A mismatch, an org-less record, an unknown
     *       record, or a registry outage is an oracle-free {@code null} — never a
     *       fallback.</li>
     * </ul>
     * Enforced identically on BOTH callers (client-credentials mint and
     * token-exchange), since both route through {@code verify()}.
     */
    private AgentIdentity verifySpireOrgBound(String credential) {
        AgentIdentity identity = defaultVerifier.verify(credential);
        if (identity == null) {
            return null;
        }
        String org = identity.orgId();
        if (org == null || org.isEmpty()) {
            // Platform-hosted / org-less SVID. REVERSE org-binding (remote-substrate
            // carry-forward), now PLACEMENT-AWARE: the refusal exists so a genuinely
            // platform SVID cannot claim a record that is meant to be authenticated
            // only by an ORG-SCOPED substrate SVID. But an org-less (platform) SVID is
            // exactly how an org MEMBER's PLATFORM-hosted agent mints: the platform
            // ClusterSPIFFEID issues org-LESS paths while that member's spawn
            // preregisters a record carrying their OrgID. That pairing is the
            // pre-substrate world and MUST keep working (spec AC 9).
            //
            // So refuse ONLY when the record's placement REQUIRES an org-scoped
            // identity — "substrate". A substrate record must be claimed solely by its
            // org-scoped substrate SVID; for platform/remote/unset placement the
            // org-less SVID IS the legitimate platform path and passes through.
            // Placement is server-set at preregistration and immutable, so a caller
            // cannot game which branch it lands in.
            //
            // This fires ONLY for identity.platformSvid() — a trust-domain-verified
            // /agent/… path in enforcing mode. In legacy passthrough or for a
            // non-SPIFFE default verifier the flag is false, so behaviour is
            // bit-identical (no getAgent, no new refusal). A registry outage
            // (getAgent==null, indistinguishable from unknown) PASSES THROUGH,
            // preserving platform-agent availability: the breach can only complete if
            // the mint then loads the substrate record, which needs the registry up.
            if (identity.platformSvid()) {
                AgentRecord record = registry.getAgent(identity.agentId());
                // Refuse on placement ALONE (not also gated on a non-empty orgId): a
                // substrate-placed record must be claimed only by its org-scoped
                // substrate SVID. Strictly more fail-closed, and symmetric with the Go
                // registry verifier (internal/registrant/orgaware.go).
                if (record != null && AgentRecord.PLACEMENT_SUBSTRATE.equals(record.placement())) {
                    return null; // substrate-placed record + platform SVID ⇒ refuse
                }
            }
            return identity;
        }
        // Org-scoped SVID: bind the signed-path org to the record's org. The
        // agentId is SVID-attested, so requiring the record it names to carry the
        // SAME org is exactly the isolation check.
        AgentRecord record = registry.getAgent(identity.agentId());
        if (record == null) {
            // Unknown record OR registry outage ⇒ fail closed.
            return null;
        }
        String recordOrg = record.orgId();
        if (recordOrg == null || recordOrg.isEmpty() || !recordOrg.equals(org)) {
            // Org-less record (no org to match a scoped SVID) or cross-org ⇒ refuse.
            return null;
        }
        return identity;
    }

    /**
     * The STS org-bound path: replay (peek+verify in one round-trip), look up the
     * record, then BIND the AWS-attested account + ARN to the record's org anchor.
     */
    private AgentIdentity verifyStsOrgBound(String credential) {
        AgentIdentity identity;
        try {
            identity = stsVerifier.verify(credential);
        } catch (RuntimeException e) {
            log.debugf(e, "org-aware STS verify threw");
            identity = null;
        }
        if (identity == null) {
            return null;
        }
        String agentId = identity.agentId();
        String arn = identity.subject();
        String account = StsCredentialVerifier.accountFromArn(arn);

        AgentRecord record = registry.getAgent(agentId);
        if (record == null) {
            // Unknown agent OR registry outage — fail closed (same null as any other).
            return null;
        }
        String orgId = record.orgId();
        if (orgId == null || orgId.isEmpty()) {
            // Org-less agent: no org anchor governs it, so the platform DEFAULT
            // verifier makes the authoritative decision (kept symmetric with the Go
            // registrant.OrgAwareVerifier). On a SPIRE-default platform the default
            // rejects an STS credential (not a JWT-SVID) ⇒ refused, preserving the
            // isolation invariant that an STS agent must belong to an anchored org;
            // on an aws-sts-default deployment the default verifies it org-less,
            // preserving the pluggable-attestation behaviour.
            return defaultVerifier.verify(credential);
        }
        Organization org = registry.getOrg(orgId);
        if (org == null) {
            // getOrg outage / unknown org ⇒ fail CLOSED (never mint against an
            // unknown anchor).
            return null;
        }
        AttestationAnchor anchor = org.anchor();
        if (anchor == null || !anchor.isActiveSts()) {
            // Unanchored or pending anchor ⇒ refuse. NO fallback to the default.
            return null;
        }
        // Bind the AWS-attested identity to THIS record's org anchor.
        if (account == null || !account.equals(anchor.awsAccountId())) {
            return null;
        }
        if (!StsCredentialVerifier.matchRoleArnPattern(anchor.roleArnPattern(), arn)) {
            return null;
        }
        return identity;
    }

    /**
     * True when {@code credential} is a base64-encoded presigned GetCallerIdentity
     * envelope (has a method + url). A JWT-SVID carries '.' separators that fail
     * standard base64 decoding, so this never misclassifies a SPIRE credential —
     * the SPIRE path stays bit-identical. Reuses the STS verifier's own decoder so
     * the shape test is exactly what the STS replay would accept.
     */
    static boolean looksLikeStsCredential(String credential) {
        if (credential == null || credential.isEmpty()) {
            return false;
        }
        StsCredentialVerifier.PresignedRequest pr = StsCredentialVerifier.decodePresigned(credential);
        return pr != null && pr.url != null && !pr.url.isEmpty()
                && pr.method != null && !pr.method.isEmpty();
    }
}
