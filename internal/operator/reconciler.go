// internal/operator/reconciler.go
package operator

import (
	"context"
	"encoding/json"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	agentv1alpha1 "github.com/agent-platform/poc/api/v1alpha1"
	"github.com/agent-platform/poc/internal/events"
	"github.com/agent-platform/poc/internal/registry"
)

const finalizer = "agent-platform.io/cleanup"

type AgentWorkloadReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Registry     registry.Client
	RegistryURL  string
	ISTokenURL   string
	SampleAPIURL string
	EventsClient events.Client // may be nil
}

func mustMarshal(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (r *AgentWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var aw agentv1alpha1.AgentWorkload
	if err := r.Get(ctx, req.NamespacedName, &aw); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !aw.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &aw)
	}
	if !controllerutil.ContainsFinalizer(&aw, finalizer) {
		controllerutil.AddFinalizer(&aw, finalizer)
		if err := r.Update(ctx, &aw); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	logger.Info("reconciling", "phase", aw.Status.Phase, "agentType", aw.Spec.AgentType)
	switch aw.Status.Phase {
	case "":
		return r.handleNew(ctx, &aw)
	case "Running":
		return r.handleRunning(ctx, &aw)
	default:
		return ctrl.Result{}, nil
	}
}

func (r *AgentWorkloadReconciler) handleNew(ctx context.Context, aw *agentv1alpha1.AgentWorkload) (ctrl.Result, error) {
	tpl, err := r.Registry.GetTemplate(ctx, aw.Spec.AgentType)
	if err != nil {
		return ctrl.Result{}, err
	}

	pod := r.buildPod(aw, tpl)
	if err := controllerutil.SetControllerReference(aw, pod, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	if r.EventsClient != nil {
		_ = r.EventsClient.PostEvent(ctx, aw.Name, events.Event{
			Source: events.SourceOperator,
			Type:   "pod_created",
			Payload: mustMarshal(map[string]string{
				"podName":   pod.Name,
				"agentType": aw.Spec.AgentType,
				"tenantId":  aw.Spec.TenantID,
				"task":      aw.Spec.Task,
			}),
		})
	}

	if aw.Spec.Lifecycle == "long-lived" {
		svc := r.buildService(aw)
		if err := controllerutil.SetControllerReference(aw, svc, r.Scheme); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, err
		}
	}

	aw.Status.Phase = "Running"
	aw.Status.PodName = pod.Name
	return ctrl.Result{}, r.Status().Update(ctx, aw)
}

func (r *AgentWorkloadReconciler) handleRunning(ctx context.Context, aw *agentv1alpha1.AgentWorkload) (ctrl.Result, error) {
	var pod corev1.Pod
	key := types.NamespacedName{Name: aw.Status.PodName, Namespace: aw.Namespace}
	if err := r.Get(ctx, key, &pod); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if pod.Status.Phase == corev1.PodFailed {
		return r.handleCompletion(ctx, aw, true)
	}
	if pod.Status.Phase == corev1.PodSucceeded && aw.Spec.Lifecycle != "long-lived" {
		return r.handleCompletion(ctx, aw, false)
	}
	return ctrl.Result{}, nil
}

func (r *AgentWorkloadReconciler) buildService(aw *agentv1alpha1.AgentWorkload) *corev1.Service {
	port := intstr.FromInt32(8080)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aw.Name + "-svc",
			Namespace: aw.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"agent-id": aw.Name},
			Ports: []corev1.ServicePort{{
				Port:       8080,
				TargetPort: port,
			}},
		},
	}
}

func (r *AgentWorkloadReconciler) handleCompletion(ctx context.Context, aw *agentv1alpha1.AgentWorkload, failed bool) (ctrl.Result, error) {
	if failed {
		_ = r.Registry.Fail(ctx, aw.Name)
		aw.Status.Phase = "Failed"
	} else {
		_ = r.Registry.Complete(ctx, aw.Name)
		aw.Status.Phase = "Completed"
	}
	return ctrl.Result{}, r.Status().Update(ctx, aw)
}

func (r *AgentWorkloadReconciler) handleDeletion(ctx context.Context, aw *agentv1alpha1.AgentWorkload) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(aw, finalizer) {
		_ = r.Registry.Fail(ctx, aw.Name)
		controllerutil.RemoveFinalizer(aw, finalizer)
		return ctrl.Result{}, r.Update(ctx, aw)
	}
	return ctrl.Result{}, nil
}

func (r *AgentWorkloadReconciler) buildPod(aw *agentv1alpha1.AgentWorkload, tpl registry.AgentTemplate) *corev1.Pod {
	sharedEnv := []corev1.EnvVar{
		{Name: "TENANT_ID", Value: aw.Spec.TenantID},
		{Name: "USER_ID", Value: aw.Spec.UserID},
		{Name: "AGENT_TYPE", Value: aw.Spec.AgentType},
		{Name: "AGENT_ID", Value: aw.Name},
		{Name: "REGISTRY_URL", Value: r.RegistryURL},
		{Name: "IS_TOKEN_URL", Value: r.ISTokenURL},
	}

	agentEnv := append([]corev1.EnvVar{
		{Name: "SAMPLE_API_URL", Value: r.SampleAPIURL},
		{Name: "SPIFFE_ENDPOINT_SOCKET", Value: "unix:///spiffe-workload-api/spire-agent.sock"},
	}, sharedEnv...)
	for k, v := range tpl.Runtime.EnvDefaults {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: k, Value: v})
	}
	if aw.Spec.Task != "" {
		agentEnv = append(agentEnv, corev1.EnvVar{Name: "TASK", Value: aw.Spec.Task})
	}

	resources := corev1.ResourceRequirements{}
	if tpl.Runtime.Resources.CPULimit != "" {
		resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(tpl.Runtime.Resources.CPULimit),
			corev1.ResourceMemory: resource.MustParse(tpl.Runtime.Resources.MemoryLimit),
		}
	}

	readOnly := true
	restartAlways := corev1.ContainerRestartPolicyAlways
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      aw.Name + "-pod",
			Namespace: aw.Namespace,
			Labels: map[string]string{
				"agent-id":                  aw.Name,
				"agent-type":                aw.Spec.AgentType,
				"tenant-id":                 aw.Spec.TenantID,
				"user-id":                   aw.Spec.UserID,
				"agent-platform.io/managed": "true",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Native sidecar (Kubernetes 1.29+ stable): restartPolicy:Always in initContainers
			// keeps the sidecar running alongside the main container but does not block pod
			// completion when the main container exits.
			InitContainers: []corev1.Container{{
				Name:            "agent-sidecar",
				Image:           "agent-sidecar:latest",
				ImagePullPolicy: corev1.PullIfNotPresent,
				RestartPolicy:   &restartAlways,
				Env:             sharedEnv,
				VolumeMounts: []corev1.VolumeMount{{
					Name:      "spiffe-workload-api",
					MountPath: "/spiffe-workload-api",
					ReadOnly:  true,
				}},
			}},
			Containers: []corev1.Container{{
				Name:            "agent",
				Image:           tpl.Runtime.Image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Env:             agentEnv,
				Resources:       resources,
			}},
			Volumes: []corev1.Volume{{
				Name: "spiffe-workload-api",
				VolumeSource: corev1.VolumeSource{
					CSI: &corev1.CSIVolumeSource{
						Driver:   "csi.spiffe.io",
						ReadOnly: &readOnly,
					},
				},
			}},
		},
	}
}

func (r *AgentWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentv1alpha1.AgentWorkload{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
