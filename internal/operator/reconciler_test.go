// internal/operator/reconciler_test.go
package operator_test

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/operator"
	"github.com/agent-platform/poc/internal/registry"
)

func buildScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := agentv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func workerTemplate() registry.AgentTemplate {
	return registry.AgentTemplate{
		AgentType: "worker",
		Version:   "1.0.0",
		Status:    "active",
		Runtime: registry.RuntimeSpec{
			Image:       "agent-runner:latest",
			Resources:   registry.ResourceLimits{CPULimit: "500m", MemoryLimit: "256Mi"},
			EnvDefaults: map[string]string{"LOG_LEVEL": "info"},
		},
		AuthZ: registry.AuthZSpec{SpiceDBRelations: []registry.SpiceDBRelationTemplate{
			{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
		}},
	}
}

func TestReconcileNew_PullsTemplateAndCreatesPod(t *testing.T) {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: agentv1alpha1.AgentWorkloadSpec{
			AgentType: "worker", UserID: "user-1", TenantID: "tenant-1", Lifecycle: "short-lived",
		},
	}

	s := buildScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(aw).WithStatusSubresource(aw).Build()

	reg := registry.NewMock(map[string]registry.AgentTemplate{"worker": workerTemplate()})

	r := &operator.AgentWorkloadReconciler{
		Client:       fakeClient,
		Scheme:       s,
		Registry:     reg,
		RegistryURL:  "http://registry:8080",
		ISTokenURL:   "http://identity-server:8080/connect/token",
		SampleAPIURL: "http://sample-api:8080",
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-agent", Namespace: "default"},
	}

	// First call adds finalizer and requeues
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (1st): %v", err)
	}

	// Second call creates pod
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (2nd): %v", err)
	}

	var pods corev1.PodList
	fakeClient.List(context.Background(), &pods)
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}
	if pods.Items[0].Spec.Containers[0].Image != "agent-runner:latest" {
		t.Errorf("unexpected image: %q", pods.Items[0].Spec.Containers[0].Image)
	}
	if pods.Items[0].Labels["agent-id"] != "test-agent" {
		t.Errorf("expected agent-id label 'test-agent', got %q", pods.Items[0].Labels["agent-id"])
	}
	if pods.Items[0].Labels["agent-platform.io/managed"] != "true" {
		t.Errorf("expected agent-platform.io/managed label 'true'")
	}
	envMap := map[string]string{}
	for _, e := range pods.Items[0].Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	for _, key := range []string{"SPIFFE_ENDPOINT_SOCKET", "REGISTRY_URL", "IS_TOKEN_URL", "AGENT_TYPE"} {
		if envMap[key] == "" {
			t.Errorf("missing env var %s in pod", key)
		}
	}

	var updated agentv1alpha1.AgentWorkload
	fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, &updated)
	if updated.Status.Phase != "Running" {
		t.Fatalf("expected Running, got %q", updated.Status.Phase)
	}
}

func TestReconcileRunning_PodSucceeded_NotifiesRegistry(t *testing.T) {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       agentv1alpha1.AgentWorkloadSpec{AgentType: "worker", TenantID: "tenant-1"},
		Status:     agentv1alpha1.AgentWorkloadStatus{Phase: "Running", PodName: "test-agent-pod"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}

	s := buildScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(aw, pod).WithStatusSubresource(aw).Build()

	reg := registry.NewMock(map[string]registry.AgentTemplate{"worker": workerTemplate()})

	r := &operator.AgentWorkloadReconciler{
		Client: fakeClient, Scheme: s, Registry: reg,
	}

	// First call adds finalizer and requeues
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-agent", Namespace: "default"},
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (1st): %v", err)
	}

	// Second call handles Running state
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (2nd): %v", err)
	}

	if len(reg.Completed) != 1 {
		t.Fatalf("registry.Complete not called: %v", reg.Completed)
	}

	var updated agentv1alpha1.AgentWorkload
	fakeClient.Get(context.Background(), types.NamespacedName{Name: "test-agent", Namespace: "default"}, &updated)
	if updated.Status.Phase != "Completed" {
		t.Fatalf("expected Completed, got %q", updated.Status.Phase)
	}
}
