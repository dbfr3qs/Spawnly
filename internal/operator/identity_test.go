package operator

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
)

func sidecarEnv(pod *corev1.Pod) map[string]string {
	out := map[string]string{}
	for _, c := range pod.Spec.InitContainers {
		if c.Name != sidecarContainerName {
			continue
		}
		for _, e := range c.Env {
			out[e.Name] = e.Value
		}
	}
	return out
}

func podWithSidecar() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: sidecarContainerName}},
		},
	}
}

func TestAwsInjector_SetsAttestorAndIdentityEnv(t *testing.T) {
	pod := podWithSidecar()
	aw := &agentv1alpha1.AgentWorkload{ObjectMeta: metav1.ObjectMeta{Name: "worker-abc123"}}

	AwsInjector{ServiceAccount: "spawnly-agent", Region: "us-east-1"}.Apply(pod, aw)

	if pod.Spec.ServiceAccountName != "spawnly-agent" {
		t.Errorf("ServiceAccountName = %q, want spawnly-agent", pod.Spec.ServiceAccountName)
	}
	env := sidecarEnv(pod)
	// ATTESTOR must reach the sidecar or it falls back to the SPIFFE workload API.
	if env["ATTESTOR"] != "aws-sts" {
		t.Errorf("sidecar ATTESTOR = %q, want aws-sts", env["ATTESTOR"])
	}
	if env["AWS_ROLE_SESSION_NAME"] != "worker-abc123" {
		t.Errorf("AWS_ROLE_SESSION_NAME = %q, want the agentId", env["AWS_ROLE_SESSION_NAME"])
	}
	if env["AWS_REGION"] != "us-east-1" {
		t.Errorf("AWS_REGION = %q, want us-east-1", env["AWS_REGION"])
	}
}

func TestSpiffeInjector_MountsWorkloadAPIAndScope(t *testing.T) {
	pod := podWithSidecar()
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-xyz"},
		Spec:       agentv1alpha1.AgentWorkloadSpec{TenantID: "acme"},
	}

	SpiffeInjector{}.Apply(pod, aw)

	if pod.Labels["agent-platform.io/scope"] != "tenant" {
		t.Errorf("scope label = %q, want tenant", pod.Labels["agent-platform.io/scope"])
	}
	if sidecarEnv(pod)["SPIFFE_ENDPOINT_SOCKET"] == "" {
		t.Error("SPIFFE_ENDPOINT_SOCKET not set on sidecar")
	}
}
