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
	"github.com/agent-platform/poc/internal/events"
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
		EventsClient: events.NewMock(),
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
	pod := pods.Items[0]
	// The agent runs as the sole regular container; the sidecar is a native
	// init container (restartPolicy:Always) so it can't block pod completion.
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[0].Name != "agent" {
		t.Errorf("expected container named 'agent', got %q", pod.Spec.Containers[0].Name)
	}
	if pod.Spec.Containers[0].Image != "agent-runner:latest" {
		t.Errorf("unexpected image: %q", pod.Spec.Containers[0].Image)
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].Name != "agent-sidecar" {
		t.Fatalf("expected init container 'agent-sidecar', got %+v", pod.Spec.InitContainers)
	}
	for _, vm := range pod.Spec.Containers[0].VolumeMounts {
		if vm.Name == "spiffe-workload-api" {
			t.Errorf("agent container should not have spiffe-workload-api volume mount")
		}
	}
	sidecarHasSpiffe := false
	for _, vm := range pod.Spec.InitContainers[0].VolumeMounts {
		if vm.Name == "spiffe-workload-api" {
			sidecarHasSpiffe = true
		}
	}
	if !sidecarHasSpiffe {
		t.Errorf("agent-sidecar container is missing spiffe-workload-api volume mount")
	}
	labels := pod.Labels
	if labels["agent-id"] != "test-agent" {
		t.Errorf("expected agent-id label 'test-agent', got %q", labels["agent-id"])
	}
	if labels["agent-type"] != "worker" {
		t.Errorf("expected agent-type label 'worker', got %q", labels["agent-type"])
	}
	if labels["tenant-id"] != "tenant-1" {
		t.Errorf("expected tenant-id label 'tenant-1', got %q", labels["tenant-id"])
	}
	if labels["user-id"] != "user-1" {
		t.Errorf("expected user-id label 'user-1', got %q", labels["user-id"])
	}
	if labels["agent-platform.io/managed"] != "true" {
		t.Errorf("expected agent-platform.io/managed label 'true'")
	}
	agentEnvMap := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		agentEnvMap[e.Name] = e.Value
	}
	for _, key := range []string{"REGISTRY_URL", "IS_TOKEN_URL", "AGENT_TYPE"} {
		if agentEnvMap[key] == "" {
			t.Errorf("missing env var %s in agent container", key)
		}
	}
	sidecarEnvMap := map[string]string{}
	for _, e := range pod.Spec.InitContainers[0].Env {
		sidecarEnvMap[e.Name] = e.Value
	}
	for _, key := range []string{"AGENT_ID", "AGENT_TYPE", "TENANT_ID", "USER_ID", "REGISTRY_URL", "IS_TOKEN_URL"} {
		if sidecarEnvMap[key] == "" {
			t.Errorf("missing env var %s in agent-sidecar container", key)
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
		EventsClient: events.NewMock(),
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

func TestReconcileNew_EmitsPodCreatedEvent(t *testing.T) {
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
	mockEvents := events.NewMock()

	r := &operator.AgentWorkloadReconciler{
		Client:       fakeClient,
		Scheme:       s,
		Registry:     reg,
		RegistryURL:  "http://registry:8080",
		ISTokenURL:   "http://identity-server:8080/connect/token",
		SampleAPIURL: "http://sample-api:8080",
		EventsClient: mockEvents,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-agent", Namespace: "default"},
	}

	// First call adds finalizer
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (1st): %v", err)
	}
	// Second call creates pod and emits event
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (2nd): %v", err)
	}

	evts := mockEvents.Events["test-agent"]
	if len(evts) != 1 {
		t.Fatalf("expected 1 event, got %d", len(evts))
	}
	if evts[0].Type != "pod_created" {
		t.Errorf("expected event type 'pod_created', got %q", evts[0].Type)
	}
	if evts[0].Source != events.SourceOperator {
		t.Errorf("expected source %q, got %q", events.SourceOperator, evts[0].Source)
	}
}

func TestReconcileNew_WithTask_SetsEnvVar(t *testing.T) {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "task-agent", Namespace: "default"},
		Spec: agentv1alpha1.AgentWorkloadSpec{
			AgentType: "worker", UserID: "user-1", TenantID: "tenant-1", Lifecycle: "short-lived",
			Task: "hello world",
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
		EventsClient: events.NewMock(),
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "task-agent", Namespace: "default"},
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (1st): %v", err)
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (2nd): %v", err)
	}

	var pod corev1.Pod
	if err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "task-agent-pod", Namespace: "default"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	envMap := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}
	if envMap["TASK"] != "hello world" {
		t.Errorf("expected TASK='hello world', got %q", envMap["TASK"])
	}
}

func TestReconcileNew_NilEventsClient_NoPanic(t *testing.T) {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{Name: "nil-events-agent", Namespace: "default"},
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
		EventsClient: nil,
	}

	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "nil-events-agent", Namespace: "default"},
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile (1st): %v", err)
	}
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("reconcile (2nd): %v", err)
	}
	if result != (ctrl.Result{}) {
		t.Errorf("expected empty Result, got %+v", result)
	}
}

const testFinalizer = "agent-platform.io/cleanup"

// A long-lived child killed by its parent right after serving its A2A reply has
// a Running/Terminating pod (never Succeeded) — it must be recorded completed,
// not failed.
func TestReconcileDeletion_RunningPod_MarksCompleted(t *testing.T) {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name: "child-agent-x", Namespace: "default",
			Finalizers: []string{testFinalizer},
		},
		Spec:   agentv1alpha1.AgentWorkloadSpec{AgentType: "worker", TenantID: "tenant-1", Lifecycle: "long-lived"},
		Status: agentv1alpha1.AgentWorkloadStatus{Phase: "Running", PodName: "child-agent-x-pod"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "child-agent-x-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	s := buildScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(aw, pod).WithStatusSubresource(aw).Build()
	reg := registry.NewMock(map[string]registry.AgentTemplate{"worker": workerTemplate()})
	r := &operator.AgentWorkloadReconciler{Client: fakeClient, Scheme: s, Registry: reg, EventsClient: events.NewMock()}

	// Deleting an object that has a finalizer sets DeletionTimestamp instead of
	// removing it, which routes Reconcile into handleDeletion.
	if err := fakeClient.Delete(context.Background(), aw); err != nil {
		t.Fatalf("delete: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "child-agent-x", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(reg.Completed) != 1 || reg.Completed[0] != "child-agent-x" {
		t.Fatalf("expected Complete(child-agent-x), got Completed=%v Failed=%v", reg.Completed, reg.Failed)
	}
	if len(reg.Failed) != 0 {
		t.Fatalf("expected no Fail calls, got %v", reg.Failed)
	}
}

// A pod that genuinely failed must still be recorded failed on deletion.
func TestReconcileDeletion_FailedPod_MarksFailed(t *testing.T) {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name: "crashed-agent", Namespace: "default",
			Finalizers: []string{testFinalizer},
		},
		Spec:   agentv1alpha1.AgentWorkloadSpec{AgentType: "worker", TenantID: "tenant-1"},
		Status: agentv1alpha1.AgentWorkloadStatus{Phase: "Running", PodName: "crashed-agent-pod"},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "crashed-agent-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	s := buildScheme(t)
	fakeClient := fake.NewClientBuilder().
		WithScheme(s).WithObjects(aw, pod).WithStatusSubresource(aw).Build()
	reg := registry.NewMock(map[string]registry.AgentTemplate{"worker": workerTemplate()})
	r := &operator.AgentWorkloadReconciler{Client: fakeClient, Scheme: s, Registry: reg, EventsClient: events.NewMock()}

	if err := fakeClient.Delete(context.Background(), aw); err != nil {
		t.Fatalf("delete: %v", err)
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "crashed-agent", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if len(reg.Failed) != 1 || reg.Failed[0] != "crashed-agent" {
		t.Fatalf("expected Fail(crashed-agent), got Completed=%v Failed=%v", reg.Completed, reg.Failed)
	}
}
