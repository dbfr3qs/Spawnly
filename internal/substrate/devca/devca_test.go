package devca

import (
	"strings"
	"testing"
	"time"
)

const testTD = "cluster.local"

func TestValidateOrgID(t *testing.T) {
	ok := []string{"acme", "globex", "org-1", "a", "a.b_c-d", "ABC123"}
	for _, s := range ok {
		if err := ValidateOrgID(s); err != nil {
			t.Errorf("ValidateOrgID(%q) = %v, want nil", s, err)
		}
	}
	// The security-relevant rejections: anything that could reshape a path or
	// inject template metacharacters.
	bad := []string{"", "a/agent/x", "../evil", "org id", "a{{b}}", "-lead", "trail-", "a\nb", "org/", "/org"}
	for _, s := range bad {
		if err := ValidateOrgID(s); err == nil {
			t.Errorf("ValidateOrgID(%q) = nil, want error", s)
		}
	}
}

func TestIssueOrgLeaf_OrgBoundAndVerifies(t *testing.T) {
	ca, err := GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	leaf, err := IssueOrgLeaf(ca, testTD, "acme", time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgLeaf: %v", err)
	}

	// The org the server template reads (CommonName) is exactly the requested org.
	org, err := OrgFromLeaf(leaf.CertPEM)
	if err != nil {
		t.Fatalf("OrgFromLeaf: %v", err)
	}
	if org != "acme" {
		t.Fatalf("OrgFromLeaf = %q, want acme", org)
	}

	// A documentary org-scoped SAN is present.
	cert, err := parseCert(leaf.CertPEM)
	if err != nil {
		t.Fatalf("parseCert: %v", err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://cluster.local/org/acme/substrate" {
		t.Fatalf("leaf URIs = %v, want [spiffe://cluster.local/org/acme/substrate]", cert.URIs)
	}

	// It chains to the issuing CA (the positive of the x509pop check).
	if err := VerifyLeaf(ca, leaf.CertPEM); err != nil {
		t.Fatalf("VerifyLeaf(own CA): %v", err)
	}
}

// AC 1 negative: a leaf signed by a DIFFERENT/forged CA must fail verification
// against the dev CA the server trusts — node attestation would reject it.
func TestVerifyLeaf_ForgedCARejected(t *testing.T) {
	realCA, err := GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA(real): %v", err)
	}
	forgedCA, err := GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA(forged): %v", err)
	}
	// Attacker forges an "acme" leaf off their OWN CA — same CommonName, wrong root.
	forgedLeaf, err := IssueOrgLeaf(forgedCA, testTD, "acme", time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgLeaf(forged): %v", err)
	}
	if err := VerifyLeaf(realCA, forgedLeaf.CertPEM); err == nil {
		t.Fatal("VerifyLeaf accepted a leaf signed by a forged CA, want rejection")
	}
}

func TestIssueOrgLeaf_RejectsBadOrg(t *testing.T) {
	ca, err := GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	if _, err := IssueOrgLeaf(ca, testTD, "a/agent/x", time.Hour); err == nil {
		t.Fatal("IssueOrgLeaf accepted a path-smuggling org id, want rejection")
	}
	if _, err := IssueOrgLeaf(ca, "", "acme", time.Hour); err == nil {
		t.Fatal("IssueOrgLeaf accepted an empty trust domain, want rejection")
	}
}

func TestLoadCA_RoundTrip(t *testing.T) {
	ca, err := GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		t.Fatalf("KeyPEM: %v", err)
	}
	reloaded, err := LoadCA(ca.CertPEM, keyPEM)
	if err != nil {
		t.Fatalf("LoadCA: %v", err)
	}
	// A leaf issued off the reloaded CA still verifies against the original — the
	// key survived the PEM round-trip.
	leaf, err := IssueOrgLeaf(reloaded, testTD, "globex", time.Hour)
	if err != nil {
		t.Fatalf("IssueOrgLeaf(reloaded): %v", err)
	}
	if err := VerifyLeaf(ca, leaf.CertPEM); err != nil {
		t.Fatalf("VerifyLeaf after reload: %v", err)
	}
}

func TestNodeSpiffePathPrefix(t *testing.T) {
	got, err := NodeSpiffePathPrefix("cluster.local", "acme")
	if err != nil {
		t.Fatalf("NodeSpiffePathPrefix: %v", err)
	}
	want := "spiffe://cluster.local/spire/agent/x509pop/org/acme/"
	if got != want {
		t.Fatalf("NodeSpiffePathPrefix = %q, want %q", got, want)
	}
	// The template constant embeds the CommonName exactly where the prefix expects
	// the org — a guard that the two stay consistent.
	if !strings.Contains(NodeAttestorPathTemplate, "/x509pop/org/{{ .Subject.CommonName }}/") {
		t.Fatalf("NodeAttestorPathTemplate %q does not embed CommonName under the org path", NodeAttestorPathTemplate)
	}
	if _, err := NodeSpiffePathPrefix("cluster.local", "bad/org"); err == nil {
		t.Fatal("NodeSpiffePathPrefix accepted a bad org id, want rejection")
	}
}

// TestNodeAttestorPathTemplate_NoSpireAgentPrefix pins SPIRE's ACTUAL node-id
// semantics, not merely our own self-consistency. SPIRE roots every attested node
// at spiffe://<td>/spire/agent and APPENDS the rendered agent_path_template. An
// earlier revision baked "/spire/agent" into the template too, producing
//
//	spiffe://cluster.local/spire/agent/spire/agent/x509pop/org/acme/<fp>
//
// which no NodeSpiffePathPrefix lookup could ever match — so entry parenting found
// no node and the live dispatch path silently failed, while the old unit tests
// still passed because they only checked the template against itself. This test
// reconstructs the id the way SPIRE does and asserts it equals what the rest of
// the code expects.
func TestNodeAttestorPathTemplate_NoSpireAgentPrefix(t *testing.T) {
	if strings.HasPrefix(NodeAttestorPathTemplate, "/spire/agent") {
		t.Fatalf("NodeAttestorPathTemplate %q must NOT begin with /spire/agent — SPIRE prepends it, doubling the node id",
			NodeAttestorPathTemplate)
	}
	// Reconstruct exactly as SPIRE does: <td root>/spire/agent + rendered template.
	const td, org, fp = "cluster.local", "acme", "deadbeef"
	rendered := strings.NewReplacer(
		"{{ .Subject.CommonName }}", org,
		"{{ .Fingerprint }}", fp,
	).Replace(NodeAttestorPathTemplate)
	spireBuilt := "spiffe://" + td + "/spire/agent" + rendered

	prefix, err := NodeSpiffePathPrefix(td, org)
	if err != nil {
		t.Fatalf("NodeSpiffePathPrefix: %v", err)
	}
	if want := prefix + fp; spireBuilt != want {
		t.Fatalf("SPIRE would attest node %q, but the code looks for %q", spireBuilt, want)
	}
}
