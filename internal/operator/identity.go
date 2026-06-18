package operator

import (
	corev1 "k8s.io/api/core/v1"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
)

// sidecarContainerName is the name of the init (native sidecar) container that
// fetches the agent's attestation credential. The IdentityInjector mounts the
// credential delivery into this container.
const sidecarContainerName = "agent-sidecar"

// IdentityInjector wires an attestor's workload-identity delivery into a pod the
// operator is about to create: the volume(s) that surface the credential to the
// sidecar, the sidecar mount + env, and the pod labels/annotations the
// attestor's control plane selects on. Implementations are chosen by the
// operator's ATTESTOR config, keeping buildPod attestor-neutral.
type IdentityInjector interface {
	// Apply mutates pod in place to deliver identity to the sidecar container.
	Apply(pod *corev1.Pod, aw *agentv1alpha1.AgentWorkload)
}

// SpiffeInjector delivers identity via SPIRE: a csi.spiffe.io volume surfaces
// the workload-API socket to the sidecar, and the pod's scope label routes
// which ClusterSPIFFEID SPIRE applies (see deploy/spire/clusterspiffeid.yaml).
type SpiffeInjector struct{}

// Apply implements IdentityInjector.
func (SpiffeInjector) Apply(pod *corev1.Pod, aw *agentv1alpha1.AgentWorkload) {
	readOnly := true
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "spiffe-workload-api",
		VolumeSource: corev1.VolumeSource{
			CSI: &corev1.CSIVolumeSource{
				Driver:   "csi.spiffe.io",
				ReadOnly: &readOnly,
			},
		},
	})

	// Only the sidecar fetches the SVID, so only it needs the socket mount/env.
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name != sidecarContainerName {
			continue
		}
		pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "spiffe-workload-api",
				MountPath: "/spiffe-workload-api",
				ReadOnly:  true,
			})
		pod.Spec.InitContainers[i].Env = append(pod.Spec.InitContainers[i].Env,
			corev1.EnvVar{
				Name:  "SPIFFE_ENDPOINT_SOCKET",
				Value: "unix:///spiffe-workload-api/spire-agent.sock",
			})
	}

	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["agent-platform.io/scope"] = scopeLabel(aw.Spec.TenantID)
}

// AwsInjector delivers identity via EKS IRSA. It binds the pod to a
// ServiceAccount annotated (by Terraform) with an IAM role ARN — the EKS IRSA
// admission webhook then injects AWS_ROLE_ARN, the projected web-identity token
// volume, and AWS_WEB_IDENTITY_TOKEN_FILE automatically. The injector adds the
// pieces the webhook does not: the per-agent STS session name (= agentId, the
// AgentId the verifiers derive) and the region for STS endpoint resolution.
type AwsInjector struct {
	// ServiceAccount is the IRSA-annotated ServiceAccount agent pods run as.
	ServiceAccount string
	// Region is the AWS region used to resolve the STS endpoint.
	Region string
}

// Apply implements IdentityInjector.
func (a AwsInjector) Apply(pod *corev1.Pod, aw *agentv1alpha1.AgentWorkload) {
	pod.Spec.ServiceAccountName = a.ServiceAccount

	// Only the sidecar presents the STS credential, so set the AWS env on it.
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name != sidecarContainerName {
			continue
		}
		pod.Spec.InitContainers[i].Env = append(pod.Spec.InitContainers[i].Env,
			// Tell the sidecar which attestor to use — without this it defaults
			// to the SPIFFE workload API and waits for a socket that isn't there.
			corev1.EnvVar{Name: "ATTESTOR", Value: "aws-sts"},
			corev1.EnvVar{Name: "AWS_ROLE_SESSION_NAME", Value: aw.Name},
			corev1.EnvVar{Name: "AWS_REGION", Value: a.Region},
		)
	}
}
