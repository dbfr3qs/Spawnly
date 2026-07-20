package run.spawnly.keycloak.attestor;

import org.junit.jupiter.api.Test;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertThrows;

/**
 * The canonical SPIFFE path contract, mirrored vector-for-vector with the Go
 * source of truth in {@code internal/spiffe/path_test.go}. If these two ever
 * disagree, a substrate token's identity won't line up with the registry record.
 */
class SpiffePathTest {

    private static void ok(String in, String td, String org, String id) {
        SpiffePath.Parsed p = SpiffePath.parse(in);
        assertEquals(td, p.trustDomain(), in);
        assertEquals(org, p.orgId(), in);
        assertEquals(id, p.agentId(), in);
    }

    private static void bad(String in) {
        assertThrows(IllegalArgumentException.class, () -> SpiffePath.parse(in), in);
    }

    @Test
    void platformShapes_orgEmpty_agentIdIsLastSegment() {
        ok("spiffe://cluster.local/agent/acme/alice/weather/agent-abc", "cluster.local", "", "agent-abc");
        ok("spiffe://cluster.local/agent/weather/agent-xyz", "cluster.local", "", "agent-xyz");
        ok("spiffe://cluster.local/agent/legacy", "cluster.local", "", "legacy");
        ok("spiffe://cluster.local/agent/weather/id/", "cluster.local", "", "id"); // trailing slash
    }

    @Test
    void orgScopedShape() {
        ok("spiffe://cluster.local/org/acme/agent/t/u/type/id", "cluster.local", "acme", "id");
        ok("spiffe://cluster.local/org/acme/agent/id/", "cluster.local", "acme", "id");
        ok("spiffe://spawnly.example/org/globex/agent/id", "spawnly.example", "globex", "id");
    }

    @Test
    void spoofsAndMalformed_failClosed() {
        bad("spiffe://cluster.local/org//agent/x");   // empty-org spoof
        bad("spiffe://cluster.local/org/acme");        // org prefix, no /agent
        bad("spiffe://cluster.local/org/acme/foo/x");  // third segment not "agent"
        bad("spiffe://cluster.local/org");             // truncated
        bad("spiffe://cluster.local/org/acme/agent");  // no agent sub-path / id segment
    }

    @Test
    void orgDeeperInPath_isNotAnOrgClaim() {
        // "org" appearing after /agent/ is a plain sub-path, NOT an org claim.
        ok("spiffe://cluster.local/agent/org/x", "cluster.local", "", "x");
    }

    @Test
    void nonSpiffeAndDegenerate_rejected() {
        bad("https://cluster.local/agent/x");
        bad("");
        bad("spiffe://"); // scheme only ⇒ empty trust domain
    }
}
