package registrant

import "testing"

func tags() map[string]any {
	return map[string]any{
		"kubernetes-pod-name":        "agent-7e5fc77f-pod",
		"kubernetes-pod-uid":         "f7a73daa",
		"kubernetes-namespace":       "default",
		"kubernetes-service-account": "spawnly-agent",
		"eks-cluster-arn":            "arn:aws:eks:us-east-1:123456789012:cluster/spawnly",
	}
}

func TestPrincipalTags_Extract(t *testing.T) {
	claim := map[string]any{"principal_tags": tags(), "principal_id": "arn:...:role/x"}
	got, err := principalTags(claim)
	if err != nil || got["kubernetes-pod-name"] != "agent-7e5fc77f-pod" {
		t.Fatalf("principalTags = %v, %v", got, err)
	}
	if _, err := principalTags(map[string]any{}); err == nil {
		t.Fatal("expected error when principal_tags missing (no Pod Identity)")
	}
	if _, err := principalTags("not-an-object"); err == nil {
		t.Fatal("expected error when claim is not an object")
	}
}

func TestIdentityFromTags_AgentIdFromAttestedPodName(t *testing.T) {
	cfg := StsWebConfig{
		Namespace:      "default",
		ServiceAccount: "spawnly-agent",
		ClusterARN:     "arn:aws:eks:us-east-1:123456789012:cluster/spawnly",
	}
	id, err := cfg.identityFromTags(tags())
	if err != nil {
		t.Fatalf("identityFromTags: %v", err)
	}
	if id.AgentID != "agent-7e5fc77f" {
		t.Errorf("AgentID = %q, want agent-7e5fc77f", id.AgentID)
	}
	if id.Issuer != "aws-stsweb" {
		t.Errorf("Issuer = %q", id.Issuer)
	}
	// Subject must be path.Base-able back to the agentId (act-chain contract).
	if want := cfg.ClusterARN + "/agent/agent-7e5fc77f"; id.Subject != want {
		t.Errorf("Subject = %q, want %q", id.Subject, want)
	}
}

func TestIdentityFromTags_RejectsMismatch(t *testing.T) {
	cfg := StsWebConfig{ServiceAccount: "expected-sa"}
	if _, err := cfg.identityFromTags(tags()); err == nil {
		t.Fatal("expected rejection when service account doesn't match")
	}
}

func TestIdentityFromTags_RejectsMissingPodName(t *testing.T) {
	cfg := StsWebConfig{}
	if _, err := cfg.identityFromTags(map[string]any{"kubernetes-namespace": "default"}); err == nil {
		t.Fatal("expected error when kubernetes-pod-name missing")
	}
}

// Spoof resistance: a malicious agent that passes a forged kubernetes-pod-name as
// a caller tag has it land in `request_tags` (a sibling of `principal_tags`). The
// verifier reads only the EKS-attested `principal_tags`, so the forged value is
// ignored and the real agent id is derived. This is the core security property of
// the aws-stsweb attestor.
func TestVerify_IgnoresForgedRequestTags(t *testing.T) {
	claim := map[string]any{
		"principal_id": "arn:aws:iam::123456789012:role/spawnly-agent",
		// EKS-set, cluster-attested — the workload cannot influence these.
		"principal_tags": map[string]any{
			"kubernetes-pod-name":        "agent-real-pod",
			"kubernetes-namespace":       "default",
			"kubernetes-service-account": "spawnly-agent",
			"eks-cluster-arn":            "arn:aws:eks:us-east-1:123456789012:cluster/spawnly",
		},
		// Caller-supplied forgery — must be ignored.
		"request_tags": map[string]any{
			"kubernetes-pod-name": "agent-evil-imposter-pod",
		},
	}

	tags, err := principalTags(claim)
	if err != nil {
		t.Fatalf("principalTags: %v", err)
	}
	id, err := StsWebConfig{}.identityFromTags(tags)
	if err != nil {
		t.Fatalf("identityFromTags: %v", err)
	}
	if id.AgentID != "agent-real" {
		t.Fatalf("AgentID = %q; the forged request_tags value must be ignored (want agent-real)", id.AgentID)
	}
}
