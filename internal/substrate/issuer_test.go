package substrate

import (
	"strings"
	"testing"
	"time"

	"github.com/spawnly/platform/internal/substrate/devca"
)

func newTestIssuer(t *testing.T) *Issuer {
	t.Helper()
	ca, err := devca.GenerateCA(time.Hour)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	iss, err := NewIssuer(ca, "cluster.local", time.Hour)
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	return iss
}

func TestIssueOrgIdentity_OrgBound(t *testing.T) {
	iss := newTestIssuer(t)
	issued, err := iss.IssueOrgIdentity("acme")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	// The leaf's CommonName (what the SPIRE x509pop template binds on) is the org.
	org, err := iss.OrgID(issued.Leaf.CertPEM)
	if err != nil {
		t.Fatalf("read leaf org: %v", err)
	}
	if org != "acme" {
		t.Fatalf("leaf org = %q, want acme", org)
	}
	if issued.CertFingerprint == "" || issued.CertSerial == "" {
		t.Fatal("fingerprint/serial must be populated")
	}
	// The node id must be under the org's x509pop namespace.
	prefix, _ := devca.NodeSpiffePathPrefix("cluster.local", "acme")
	if !strings.HasPrefix(issued.NodeID, prefix) {
		t.Fatalf("node id %q not under org prefix %q", issued.NodeID, prefix)
	}
	if strings.TrimPrefix(issued.NodeID, prefix) == "" {
		t.Fatal("node id has no fingerprint segment")
	}
}

func TestIssueOrgIdentity_NormalizesCase(t *testing.T) {
	iss := newTestIssuer(t)
	// A mixed-case org must normalize to the lowercase Keycloak slug so it can't
	// diverge into a distinct SPIFFE segment.
	issued, err := iss.IssueOrgIdentity("Acme")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	org, _ := iss.OrgID(issued.Leaf.CertPEM)
	if org != "acme" {
		t.Fatalf("expected normalized org 'acme', got %q", org)
	}
}

func TestIssueOrgIdentity_RejectsBadOrg(t *testing.T) {
	iss := newTestIssuer(t)
	for _, bad := range []string{"", "a/b", "acme corp", "../evil"} {
		if _, err := iss.IssueOrgIdentity(bad); err == nil {
			t.Fatalf("expected rejection of org %q", bad)
		}
	}
}

func TestNewIssuer_FailsClosed(t *testing.T) {
	if _, err := NewIssuer(nil, "cluster.local", time.Hour); err == nil {
		t.Fatal("nil CA must be rejected")
	}
	ca, _ := devca.GenerateCA(time.Hour)
	if _, err := NewIssuer(ca, "", time.Hour); err == nil {
		t.Fatal("empty trust domain must be rejected")
	}
}
