package run.spawnly.keycloak.attestor;

import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

import run.spawnly.keycloak.registry.AgentRecord;
import run.spawnly.keycloak.registry.AgentRegistryClient;
import run.spawnly.keycloak.registry.AttestationAnchor;
import run.spawnly.keycloak.registry.Organization;

import java.nio.charset.StandardCharsets;
import java.util.Base64;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertNull;
import static org.junit.jupiter.api.Assertions.assertTrue;
import static org.mockito.ArgumentMatchers.any;
import static org.mockito.ArgumentMatchers.anyString;
import static org.mockito.Mockito.mock;
import static org.mockito.Mockito.never;
import static org.mockito.Mockito.verify;
import static org.mockito.Mockito.when;

/**
 * The per-org verifier resolution isolation matrix (spec criterion 12,
 * unit-level). The STS replay itself is mocked (it is exercised for real in
 * {@link StsCredentialVerifierTest}); these tests pin the RESOLUTION + BINDING:
 * an STS credential authenticates ONLY against the anchor of the org owning the
 * agent's record, with no cross-verifier fallback, and every failure is the same
 * oracle-free {@code null}. The platform SPIRE path (a non-STS credential) is
 * routed straight to the default verifier, unaffected by any anchor.
 */
class OrgAwareVerifierTest {

    private static final String ACME_ACCOUNT = "111122223333";
    private static final String GLOBEX_ACCOUNT = "999988887777";
    private static final String ACME_ROLE = "arn:aws:sts::111122223333:assumed-role/acme-agents/*";
    private static final String GLOBEX_ROLE = "arn:aws:sts::999988887777:assumed-role/globex-agents/*";
    // The AWS-attested ARN for the acme remote agent (session name == agentId).
    private static final String ACME_ARN = "arn:aws:sts::111122223333:assumed-role/acme-agents/remote-1";

    private AgentCredentialVerifier defaultVerifier; // SPIRE
    private AgentCredentialVerifier stsVerifier;      // the replay (mocked)
    private AgentRegistryClient registry;
    private OrgAwareVerifier orgAware;

    // A well-formed base64 presigned envelope so looksLikeStsCredential routes it
    // to the STS branch; the stsVerifier mock supplies the attested identity.
    private static final String STS_CRED = Base64.getEncoder().encodeToString(
            "{\"method\":\"POST\",\"url\":\"https://sts.amazonaws.com/?Action=GetCallerIdentity\"}"
                    .getBytes(StandardCharsets.UTF_8));

    @BeforeEach
    void setUp() {
        defaultVerifier = mock(AgentCredentialVerifier.class);
        stsVerifier = mock(AgentCredentialVerifier.class);
        registry = mock(AgentRegistryClient.class);
        orgAware = new OrgAwareVerifier(defaultVerifier, stsVerifier, registry);
        // The STS replay attests the acme remote agent by default; tests that need
        // a different attested identity re-stub it.
        when(stsVerifier.verify(anyString())).thenReturn(new AgentIdentity("remote-1", ACME_ARN, "aws-sts"));
    }

    private static AttestationAnchor activeSts(String account, String rolePattern) {
        return new AttestationAnchor("sts", account, rolePattern, "active");
    }

    private void agentInOrg(String agentId, String orgId) {
        when(registry.getAgent(agentId))
                .thenReturn(new AgentRecord(agentId, "active", "alice", "remote-worker", null, orgId));
    }

    private void org(String orgId, AttestationAnchor anchor) {
        when(registry.getOrg(orgId)).thenReturn(new Organization(orgId, orgId, anchor));
    }

    // org-A credential × org-A record ⇒ minted.
    @Test
    void stsCredMatchesOwnOrg_verified() {
        agentInOrg("remote-1", "acme");
        org("acme", activeSts(ACME_ACCOUNT, ACME_ROLE));

        AgentIdentity id = orgAware.verify(STS_CRED);

        assertNotNull(id);
        assertEquals("remote-1", id.agentId());
        assertEquals("aws-sts", id.issuer());
        // No cross-verifier fallback: the default verifier is never consulted.
        verify(defaultVerifier, never()).verify(anyString());
    }

    // org-A credential × org-B record ⇒ refused (account is not org B's anchor).
    @Test
    void stsCredCrossOrg_refused() {
        agentInOrg("remote-1", "globex");
        org("globex", activeSts(GLOBEX_ACCOUNT, GLOBEX_ROLE));

        assertNull(orgAware.verify(STS_CRED));
        verify(defaultVerifier, never()).verify(anyString());
    }

    // unanchored-org record × STS credential ⇒ refused (no anchor to ride).
    @Test
    void unanchoredOrg_refused() {
        agentInOrg("remote-1", "acme");
        org("acme", null); // no anchor

        assertNull(orgAware.verify(STS_CRED));
        verify(defaultVerifier, never()).verify(anyString());
    }

    // anchor pending ⇒ treated as unanchored ⇒ refused.
    @Test
    void pendingAnchor_refused() {
        agentInOrg("remote-1", "acme");
        org("acme", new AttestationAnchor("sts", ACME_ACCOUNT, ACME_ROLE, "pending"));

        assertNull(orgAware.verify(STS_CRED));
    }

    // role-ARN pattern mismatch ⇒ refused even when the account matches.
    @Test
    void rolePatternMismatch_refused() {
        agentInOrg("remote-1", "acme");
        org("acme", activeSts(ACME_ACCOUNT, "arn:aws:sts::111122223333:assumed-role/other-role/*"));

        assertNull(orgAware.verify(STS_CRED));
    }

    // registry/getOrg outage ⇒ fail closed (getOrg returns null on any error).
    @Test
    void getOrgOutage_failsClosed() {
        agentInOrg("remote-1", "acme");
        when(registry.getOrg("acme")).thenReturn(null);

        assertNull(orgAware.verify(STS_CRED));
    }

    // unknown record ⇒ refused.
    @Test
    void unknownRecord_refused() {
        when(registry.getAgent("remote-1")).thenReturn(null);

        assertNull(orgAware.verify(STS_CRED));
        verify(registry, never()).getOrg(anyString());
    }

    // org-less agent presenting an STS credential ⇒ the platform DEFAULT verifier
    // decides (symmetric with Go). On a SPIRE-default platform the default rejects
    // it (STS is not a JWT-SVID) ⇒ refused; it never touches the org anchor.
    @Test
    void orgLessStsAgent_delegatesToDefault_refusedOnSpireDefault() {
        when(registry.getAgent("remote-1"))
                .thenReturn(new AgentRecord("remote-1", "active", "alice", "remote-worker", null, null));
        when(defaultVerifier.verify(STS_CRED)).thenReturn(null); // SPIRE default rejects an STS cred

        assertNull(orgAware.verify(STS_CRED));
        verify(defaultVerifier).verify(STS_CRED); // delegation happened
        verify(registry, never()).getOrg(anyString()); // no anchor consulted for an org-less agent
    }

    // A bad STS credential (replay fails ⇒ mock returns null) ⇒ refused, no lookup.
    @Test
    void badStsCredential_refused() {
        when(stsVerifier.verify(anyString())).thenReturn(null);

        assertNull(orgAware.verify(STS_CRED));
        verify(registry, never()).getAgent(anyString());
    }

    // A SPIRE (JWT-SVID) credential ⇒ the platform default path, UNAFFECTED by any
    // anchor and never touching the org store.
    @Test
    void spireCredential_usesDefaultPath_unaffectedByAnchors() {
        String jwt = "header.payload.signature"; // JWT shape ⇒ not an STS envelope
        when(defaultVerifier.verify(jwt))
                .thenReturn(new AgentIdentity("agent-x", "spiffe://cluster.local/agent/agent-x", "spiffe-svid"));

        AgentIdentity id = orgAware.verify(jwt);

        assertNotNull(id);
        assertEquals("agent-x", id.agentId());
        assertEquals("spiffe-svid", id.issuer());
        verify(stsVerifier, never()).verify(anyString());
        verify(registry, never()).getAgent(anyString());
        verify(registry, never()).getOrg(anyString());
    }

    // ── SPIRE org-bound matrix (remote-substrate Phase 1, spec AC 4/5) ───────────
    // The default (SPIRE) verifier surfaces the signed-path org on the identity;
    // OrgAwareVerifier binds it to the record's org with the same fail-closed
    // discipline as the STS branch. Mirrors internal/registrant/orgaware_spire_test.go.

    // A JWT-shaped credential ⇒ routed to the SPIRE (default) branch, not STS.
    private static final String SPIRE_JWT = "header.payload.signature";

    private void spireIdentity(String agentId, String org) {
        String spiffe = org == null
                ? "spiffe://cluster.local/agent/weather/" + agentId
                : "spiffe://cluster.local/org/" + org + "/agent/t/alice/worker/" + agentId;
        when(defaultVerifier.verify(SPIRE_JWT))
                .thenReturn(new AgentIdentity(agentId, spiffe, "spiffe-svid", org));
    }

    // org-scoped SVID × matching record ⇒ verified; STS replay never consulted.
    @Test
    void spireOrgMatchesOwnOrg_verified() {
        spireIdentity("agent-a", "acme");
        when(registry.getAgent("agent-a"))
                .thenReturn(new AgentRecord("agent-a", "active", "alice", "worker", null, "acme"));

        AgentIdentity id = orgAware.verify(SPIRE_JWT);

        assertNotNull(id);
        assertEquals("agent-a", id.agentId());
        assertEquals("acme", id.orgId());
        verify(stsVerifier, never()).verify(anyString());
    }

    // org-A SVID × org-B record ⇒ refused (the named record is owned by globex).
    @Test
    void spireOrgCrossOrgRecord_refused() {
        spireIdentity("agent-b", "acme"); // SVID claims acme, names an org-B record
        when(registry.getAgent("agent-b"))
                .thenReturn(new AgentRecord("agent-b", "active", "alice", "worker", null, "globex"));

        assertNull(orgAware.verify(SPIRE_JWT));
    }

    // org-scoped SVID × org-less record ⇒ refused (no org on the record to match).
    @Test
    void spireOrgScopedAgainstOrgLessRecord_refused() {
        spireIdentity("agent-a", "acme");
        when(registry.getAgent("agent-a"))
                .thenReturn(new AgentRecord("agent-a", "active", "alice", "worker", null, null));

        assertNull(orgAware.verify(SPIRE_JWT));
    }

    // org-scoped SVID × unknown record ⇒ refused (fail closed).
    @Test
    void spireOrgUnknownRecord_refused() {
        spireIdentity("ghost", "acme");
        when(registry.getAgent("ghost")).thenReturn(null);

        assertNull(orgAware.verify(SPIRE_JWT));
    }

    // REGRESSION: an org-less (platform) SVID from a LEGACY (non-enforcing)
    // verifier — platformSvid=false — passes through, org store untouched.
    @Test
    void spirePlatformSvid_passesThrough_noStoreTouch() {
        spireIdentity("agent-x", null); // org-less platform path, platformSvid=false

        AgentIdentity id = orgAware.verify(SPIRE_JWT);

        assertNotNull(id);
        assertEquals("agent-x", id.agentId());
        assertNull(id.orgId());
        verify(registry, never()).getAgent(anyString());
        verify(stsVerifier, never()).verify(anyString());
    }

    // A genuinely-platform SVID (enforcing verifier ⇒ platformSvid=true).
    private void spirePlatformEnforcedIdentity(String agentId) {
        when(defaultVerifier.verify(SPIRE_JWT))
                .thenReturn(new AgentIdentity(agentId, "spiffe://cluster.local/agent/weather/" + agentId,
                        "spiffe-svid", null, true));
    }

    // REGRESSION (spec AC 9): a genuinely-platform SVID MUST authenticate an
    // org-scoped record that is PLATFORM-placed — that is exactly how an org
    // member's platform-hosted agent mints. The reverse-binding refusal is now
    // placement-aware and must NOT fire here.
    @Test
    void spirePlatformSvidVsPlatformPlacedOrgRecord_allowed() {
        spirePlatformEnforcedIdentity("agent-a");
        // 6-arg ctor ⇒ placement=null (== platform-hosted default).
        when(registry.getAgent("agent-a"))
                .thenReturn(new AgentRecord("agent-a", "active", "alice", "worker", null, "acme"));

        AgentIdentity id = orgAware.verify(SPIRE_JWT);
        assertNotNull(id, "a platform SVID must authenticate a PLATFORM-placed org record (AC 9)");
        assertEquals("agent-a", id.agentId());
        assertNull(id.orgId(), "an org-less SVID must stay org-less");
    }

    // PRESERVED INVARIANT: a genuinely-platform SVID must NOT authenticate a
    // SUBSTRATE-placed record — that record may be claimed only by its org-scoped
    // substrate SVID.
    @Test
    void spirePlatformSvidVsSubstrateRecord_refused() {
        spirePlatformEnforcedIdentity("sub-a");
        when(registry.getAgent("sub-a"))
                .thenReturn(new AgentRecord("sub-a", "active", "alice", "worker", null, "acme",
                        AgentRecord.PLACEMENT_SUBSTRATE));

        assertNull(orgAware.verify(SPIRE_JWT),
                "a platform SVID must not authenticate a substrate-placed record");
    }

    // A genuinely-platform SVID naming an ORG-LESS record passes through.
    @Test
    void spirePlatformSvidVsOrgLessRecord_passesThrough() {
        spirePlatformEnforcedIdentity("agent-x");
        when(registry.getAgent("agent-x"))
                .thenReturn(new AgentRecord("agent-x", "active", "alice", "worker", null, null));

        AgentIdentity id = orgAware.verify(SPIRE_JWT);
        assertNotNull(id);
        assertNull(id.orgId());
    }

    // A genuinely-platform SVID naming an UNKNOWN record passes through (may
    // self-register later) — and a registry outage is indistinguishable here, so
    // it too passes through, preserving platform-agent availability.
    @Test
    void spirePlatformSvidUnknownRecord_passesThrough() {
        spirePlatformEnforcedIdentity("ghost");
        when(registry.getAgent("ghost")).thenReturn(null);

        AgentIdentity id = orgAware.verify(SPIRE_JWT);
        assertNotNull(id);
        assertEquals("ghost", id.agentId());
    }

    // ── Substrate-edge coexistence (mint side, spec AC-5a) ───────────────────────
    // The AWS edge default verifier is aws-stsweb (platform EKS agents), yet a
    // substrate workload mints with a SPIFFE JWT-SVID. withSpiffe installs a
    // SEPARATE SPIFFE verifier used ONLY for the SPIFFE-SVID-shaped non-STS
    // credentials; the router (NOT spiffe-only) keeps platform stsweb mints working.
    // Mirrors internal/registrant/orgaware_substrate_test.go.

    /**
     * A minimal unsigned JWT (header.payload.signature) whose only claim is
     * {@code sub}. The verifier mocks ignore token content, so this drives ONLY
     * the SPIFFE-vs-stsweb ROUTING ({@link OrgAwareVerifier#looksLikeSpiffeSvid}).
     */
    private static String jwtWithSub(String sub) {
        String payload = Base64.getUrlEncoder().withoutPadding()
                .encodeToString(("{\"sub\":\"" + sub + "\"}").getBytes(StandardCharsets.UTF_8));
        return "eyJhbGciOiJSUzI1NiJ9." + payload + ".sig";
    }

    // A SPIFFE-SVID-shaped credential (spiffe:// sub) routes to the substrate
    // SPIFFE verifier, NOT the aws-stsweb default, and the org binding is applied.
    @Test
    void substrate_spiffeSvid_routedToSpiffeVerifier_orgBound() {
        AgentCredentialVerifier spiffe = mock(AgentCredentialVerifier.class);
        String cred = jwtWithSub("spiffe://spawnly.run/org/acme/agent/t/alice/worker/agent-a");
        when(spiffe.verify(cred)).thenReturn(
                new AgentIdentity("agent-a", "spiffe://spawnly.run/org/acme/agent/t/alice/worker/agent-a",
                        "spiffe-svid", "acme"));
        when(registry.getAgent("agent-a"))
                .thenReturn(new AgentRecord("agent-a", "active", "alice", "worker", null, "acme"));

        OrgAwareVerifier v = new OrgAwareVerifier(defaultVerifier, stsVerifier, registry).withSpiffe(spiffe);
        AgentIdentity id = v.verify(cred);

        assertNotNull(id);
        assertEquals("agent-a", id.agentId());
        assertEquals("acme", id.orgId());
        verify(defaultVerifier, never()).verify(anyString());
        verify(stsVerifier, never()).verify(anyString());
    }

    // THE REGRESSION GUARD (the platform-break trap): an stsweb web-identity JWT is
    // ALSO non-STS-shaped, so it shares the non-STS branch. With a spiffe verifier
    // wired it MUST still route to the aws-stsweb DEFAULT — its sub is an ARN, not a
    // spiffe:// ID — or platform token minting breaks. The spiffe branch never sees it.
    @Test
    void substrate_stswebJwt_routesToDefaultNotSpiffe() {
        AgentCredentialVerifier spiffe = mock(AgentCredentialVerifier.class);
        String cred = jwtWithSub("arn:aws:sts::123456789012:assumed-role/eks-role/pod");
        when(defaultVerifier.verify(cred))
                .thenReturn(new AgentIdentity("plat-1", "arn:aws:sts::123456789012:assumed-role/eks-role/pod",
                        "aws-stsweb"));

        OrgAwareVerifier v = new OrgAwareVerifier(defaultVerifier, stsVerifier, registry).withSpiffe(spiffe);
        AgentIdentity id = v.verify(cred);

        assertNotNull(id);
        assertEquals("plat-1", id.agentId());
        verify(defaultVerifier).verify(cred);
        verify(spiffe, never()).verify(anyString());
    }

    // An STS-presigned credential is UNAFFECTED by the spiffe branch: it still takes
    // the org-bound STS path; NEITHER the default NOR the spiffe branch is consulted.
    @Test
    void substrate_stsCred_stillTakesStsPath_spiffeNeverConsulted() {
        AgentCredentialVerifier spiffe = mock(AgentCredentialVerifier.class);
        agentInOrg("remote-1", "acme");
        org("acme", activeSts(ACME_ACCOUNT, ACME_ROLE));

        OrgAwareVerifier v = new OrgAwareVerifier(defaultVerifier, stsVerifier, registry).withSpiffe(spiffe);
        AgentIdentity id = v.verify(STS_CRED); // stsVerifier (setUp) attests remote-1

        assertNotNull(id);
        assertEquals("remote-1", id.agentId());
        assertEquals("aws-sts", id.issuer());
        verify(spiffe, never()).verify(anyString());
        verify(defaultVerifier, never()).verify(anyString());
    }

    // ISOLATION through the spiffe branch: an org-A SVID naming an org-B record is
    // refused (null), and there is NO fallback to the aws-stsweb default.
    @Test
    void substrate_spiffeBranch_crossOrgRecord_refused_noFallback() {
        AgentCredentialVerifier spiffe = mock(AgentCredentialVerifier.class);
        String cred = jwtWithSub("spiffe://spawnly.run/org/acme/agent/t/alice/worker/agent-b");
        when(spiffe.verify(cred)).thenReturn(
                new AgentIdentity("agent-b", "spiffe://spawnly.run/org/acme/agent/t/alice/worker/agent-b",
                        "spiffe-svid", "acme"));
        when(registry.getAgent("agent-b"))
                .thenReturn(new AgentRecord("agent-b", "active", "alice", "worker", null, "globex"));

        OrgAwareVerifier v = new OrgAwareVerifier(defaultVerifier, stsVerifier, registry).withSpiffe(spiffe);

        assertNull(v.verify(cred));
        verify(defaultVerifier, never()).verify(anyString());
    }

    // BACK-COMPAT: with NO spiffe verifier wired, even a spiffe:// -sub credential
    // goes to the default — an unset toggle changes nothing.
    @Test
    void substrate_nilSpiffeBranch_spiffeSubCredGoesToDefault() {
        String cred = jwtWithSub("spiffe://spawnly.run/agent/weather/agent-x");
        when(defaultVerifier.verify(cred))
                .thenReturn(new AgentIdentity("agent-x", "spiffe://spawnly.run/agent/weather/agent-x",
                        "spiffe-svid"));

        AgentIdentity id = orgAware.verify(cred); // orgAware from setUp has no withSpiffe

        assertNotNull(id);
        assertEquals("agent-x", id.agentId());
        verify(defaultVerifier).verify(cred);
    }

    @Test
    void looksLikeSpiffeSvid_detection() {
        assertTrue(OrgAwareVerifier.looksLikeSpiffeSvid(
                jwtWithSub("spiffe://spawnly.run/org/acme/agent/x")));
        // stsweb / EKS subjects ⇒ NOT spiffe ⇒ route to default.
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid(
                jwtWithSub("arn:aws:sts::123456789012:assumed-role/eks-role/pod")));
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid(
                jwtWithSub("system:serviceaccount:default:spawnly-agent")));
        // 2-part / garbage / empty / null ⇒ false.
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid("header.payload"));
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid("not-a-jwt"));
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid(""));
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid(null));
        // 3 parts but the payload has no sub claim ⇒ false.
        String noSub = "eyJhbGciOiJSUzI1NiJ9." + Base64.getUrlEncoder().withoutPadding()
                .encodeToString("{\"foo\":\"bar\"}".getBytes(StandardCharsets.UTF_8)) + ".sig";
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid(noSub));
        // 3 parts but the payload is not valid base64url/JSON ⇒ false.
        assertFalse(OrgAwareVerifier.looksLikeSpiffeSvid("a.!!!.c"));
    }

    @Test
    void looksLikeStsCredential_detection() {
        assertTrue(OrgAwareVerifier.looksLikeStsCredential(STS_CRED));
        assertFalse(OrgAwareVerifier.looksLikeStsCredential("header.payload.signature"));
        assertFalse(OrgAwareVerifier.looksLikeStsCredential(""));
        assertFalse(OrgAwareVerifier.looksLikeStsCredential(null));
        // Valid base64 but not a presigned envelope (no method/url).
        assertFalse(OrgAwareVerifier.looksLikeStsCredential(
                Base64.getEncoder().encodeToString("{\"foo\":\"bar\"}".getBytes(StandardCharsets.UTF_8))));
    }
}
