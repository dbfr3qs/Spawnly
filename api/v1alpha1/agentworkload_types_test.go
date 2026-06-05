package v1alpha1_test

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/spawnly/poc/api/v1alpha1"
)

func TestAgentWorkloadRoundtrip(t *testing.T) {
	aw := v1alpha1.AgentWorkload{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AgentWorkload",
			APIVersion: "agent-platform.io/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentWorkloadSpec{
			AgentType: "worker",
			UserID:    "user-1",
			TenantID:  "tenant-1",
			Lifecycle: "short-lived",
		},
	}

	b, err := json.Marshal(aw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got v1alpha1.AgentWorkload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Spec.AgentType != "worker" {
		t.Errorf("got AgentType %q, want %q", got.Spec.AgentType, "worker")
	}
	if got.Spec.UserID != "user-1" {
		t.Errorf("got UserID %q, want %q", got.Spec.UserID, "user-1")
	}
	if got.Spec.TenantID != "tenant-1" {
		t.Errorf("got TenantID %q, want %q", got.Spec.TenantID, "tenant-1")
	}
	if got.Spec.Lifecycle != "short-lived" {
		t.Errorf("got Lifecycle %q, want %q", got.Spec.Lifecycle, "short-lived")
	}
}
