package main

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentv1alpha1 "github.com/spawnly/platform/api/v1alpha1"
	"github.com/spawnly/platform/internal/registry"
)

// workloadStore materializes/removes/enumerates the substrate's local
// AgentWorkload CRs. It is an interface so the connect+reconcile logic is
// unit-testable against a fake, with the live impl (k8sWorkloadStore) backed by a
// controller-runtime client into the substrate namespace.
type workloadStore interface {
	// Create makes (idempotently) an AgentWorkload CR named rec.AgentID.
	Create(ctx context.Context, rec registry.AgentRecord, task string) error
	// Get resolves a local CR (its agentType, needed to rebuild the entry id for
	// teardown), returning found=false when absent.
	Get(ctx context.Context, agentID string) (localWorkload, bool, error)
	// Delete removes the CR named agentID and returns whether it existed.
	Delete(ctx context.Context, agentID string) (existed bool, err error)
	// List returns the substrate-managed local CRs.
	List(ctx context.Context) ([]localWorkload, error)
}

// k8sWorkloadStore is the live workloadStore over a controller-runtime client,
// scoped to one namespace (the substrate bundle's namespace).
type k8sWorkloadStore struct {
	c         client.Client
	namespace string
}

// managedLabel marks CRs this substrate-agent owns, so List never sweeps up a CR
// created by something else in the namespace.
const managedLabel = "agent-platform.io/substrate-managed"

func (s *k8sWorkloadStore) Create(ctx context.Context, rec registry.AgentRecord, task string) error {
	aw := &agentv1alpha1.AgentWorkload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rec.AgentID,
			Namespace: s.namespace,
			Labels:    map[string]string{managedLabel: "true"},
		},
		Spec: agentv1alpha1.AgentWorkloadSpec{
			AgentType:       rec.AgentType,
			UserID:          rec.UserID,
			TenantID:        rec.TenantID,
			Lifecycle:       rec.Lifecycle,
			Task:            task,
			ParentID:        rec.ParentID,
			ConsentRequired: rec.ConsentRequired,
		},
	}
	if err := s.c.Create(ctx, aw); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // idempotent
		}
		return fmt.Errorf("create AgentWorkload %s: %w", rec.AgentID, err)
	}
	return nil
}

func (s *k8sWorkloadStore) Get(ctx context.Context, agentID string) (localWorkload, bool, error) {
	var aw agentv1alpha1.AgentWorkload
	if err := s.c.Get(ctx, client.ObjectKey{Namespace: s.namespace, Name: agentID}, &aw); err != nil {
		if apierrors.IsNotFound(err) {
			return localWorkload{}, false, nil
		}
		return localWorkload{}, false, fmt.Errorf("get AgentWorkload %s: %w", agentID, err)
	}
	return localWorkload{AgentID: aw.Name, AgentType: aw.Spec.AgentType}, true, nil
}

func (s *k8sWorkloadStore) Delete(ctx context.Context, agentID string) (bool, error) {
	aw := &agentv1alpha1.AgentWorkload{ObjectMeta: metav1.ObjectMeta{Name: agentID, Namespace: s.namespace}}
	if err := s.c.Delete(ctx, aw); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("delete AgentWorkload %s: %w", agentID, err)
	}
	return true, nil
}

func (s *k8sWorkloadStore) List(ctx context.Context) ([]localWorkload, error) {
	var list agentv1alpha1.AgentWorkloadList
	if err := s.c.List(ctx, &list, client.InNamespace(s.namespace), client.MatchingLabels{managedLabel: "true"}); err != nil {
		return nil, fmt.Errorf("list AgentWorkloads: %w", err)
	}
	out := make([]localWorkload, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, localWorkload{AgentID: list.Items[i].Name, AgentType: list.Items[i].Spec.AgentType})
	}
	return out, nil
}
