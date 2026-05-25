package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type AgentWorkloadSpec struct {
	AgentType string `json:"agentType"`
	UserID    string `json:"userId"`
	TenantID  string `json:"tenantId"`
	Lifecycle string `json:"lifecycle"` // short-lived | long-lived
	Task      string `json:"task,omitempty"`
}

type AgentWorkloadStatus struct {
	Phase   string `json:"phase,omitempty"`   // "" | Pending | Running | Completed | Failed
	PodName string `json:"podName,omitempty"`
	AgentID string `json:"agentId,omitempty"` // short agent ID (SPIFFE path component), stored for teardown
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type AgentWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentWorkloadSpec   `json:"spec,omitempty"`
	Status            AgentWorkloadStatus `json:"status,omitempty"`
}

func (in *AgentWorkload) DeepCopyObject() runtime.Object {
	out := new(AgentWorkload)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentWorkload) DeepCopyInto(out *AgentWorkload) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	out.Status = in.Status
}

// +kubebuilder:object:root=true
type AgentWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentWorkload `json:"items"`
}

func (in *AgentWorkloadList) DeepCopyObject() runtime.Object {
	out := new(AgentWorkloadList)
	in.DeepCopyInto(out)
	return out
}

func (in *AgentWorkloadList) DeepCopyInto(out *AgentWorkloadList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]AgentWorkload, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}
