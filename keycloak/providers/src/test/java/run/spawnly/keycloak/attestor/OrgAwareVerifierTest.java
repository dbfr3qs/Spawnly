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
