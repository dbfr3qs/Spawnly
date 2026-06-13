package spicedb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/authzed/grpcutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var _ Client = (*Mock)(nil)

// Client is the interface used by operator, sample-api, and orchestrator.
// resource and subject are SpiceDB object strings: "type:id".
type Client interface {
	WriteSchema(ctx context.Context, schema string) error
	// WriteRelationship writes a single tuple: resource#relation@subject.
	WriteRelationship(ctx context.Context, resource, relation, subject string) error
	// DeleteRelationship removes a single fully-specified tuple:
	// resource#relation@subject. Unlike DeleteAgentRelationships' broad
	// subject-only filter, this targets one exact tuple — used to toggle an
	// agent's `enabled` status tuple on revoke/resume (Phase 5a).
	DeleteRelationship(ctx context.Context, resource, relation, subject string) error
	// DeleteAgentRelationships removes every tuple whose subject is "agent:agentID"
	// and whose resource is one of resourceTypes. Pass the set of resource types
	// the agent's template relations reference (e.g. {"tenant"} or
	// {"tenant", "project"}) — SpiceDB's delete filter is scoped to a single
	// resource type per call, so one call is issued per type. An empty (or nil)
	// resourceTypes slice is a no-op.
	DeleteAgentRelationships(ctx context.Context, agentID string, resourceTypes []string) error
	// CheckPermission returns true when subject holds permission on resource.
	CheckPermission(ctx context.Context, resource, permission, subject string) (bool, error)
}

// parseRef splits "type:id" into (type, id).
func parseRef(s string) (objType, objID string) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return s, ""
	}
	return parts[0], parts[1]
}

// New connects to SpiceDB at endpoint using a pre-shared key.
func New(endpoint, presharedKey string) (Client, error) {
	c, err := authzed.NewClient(
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpcutil.WithInsecureBearerToken(presharedKey),
	)
	if err != nil {
		return nil, fmt.Errorf("authzed.NewClient: %w", err)
	}
	return &realClient{c: c}, nil
}

type realClient struct{ c *authzed.Client }

func (r *realClient) WriteSchema(ctx context.Context, schema string) error {
	_, err := r.c.WriteSchema(ctx, &v1.WriteSchemaRequest{Schema: schema})
	return err
}

func (r *realClient) WriteRelationship(ctx context.Context, resource, relation, subject string) error {
	resType, resID := parseRef(resource)
	subType, subID := parseRef(subject)
	_, err := r.c.WriteRelationships(ctx, &v1.WriteRelationshipsRequest{
		Updates: []*v1.RelationshipUpdate{{
			Operation: v1.RelationshipUpdate_OPERATION_TOUCH,
			Relationship: &v1.Relationship{
				Resource: &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
				Relation: relation,
				Subject:  &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subType, ObjectId: subID}},
			},
		}},
	})
	return err
}

func (r *realClient) DeleteRelationship(ctx context.Context, resource, relation, subject string) error {
	resType, resID := parseRef(resource)
	subType, subID := parseRef(subject)
	_, err := r.c.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType:       resType,
			OptionalResourceId: resID,
			OptionalRelation:   relation,
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       subType,
				OptionalSubjectId: subID,
			},
		},
	})
	return err
}

func (r *realClient) DeleteAgentRelationships(ctx context.Context, agentID string, resourceTypes []string) error {
	var errs []error
	for _, rt := range resourceTypes {
		_, err := r.c.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
			RelationshipFilter: &v1.RelationshipFilter{
				ResourceType: rt,
				OptionalSubjectFilter: &v1.SubjectFilter{
					SubjectType:       "agent",
					OptionalSubjectId: agentID,
				},
			},
		})
		if err != nil {
			// Join rather than return early: a failure cleaning one resource
			// type must not block cleanup of the others (a partial leak on one
			// type is still reported).
			errs = append(errs, fmt.Errorf("delete %s relationships for agent %s: %w", rt, agentID, err))
		}
	}
	return errors.Join(errs...)
}

func (r *realClient) CheckPermission(ctx context.Context, resource, permission, subject string) (bool, error) {
	resType, resID := parseRef(resource)
	subType, subID := parseRef(subject)
	resp, err := r.c.CheckPermission(ctx, &v1.CheckPermissionRequest{
		Resource:   &v1.ObjectReference{ObjectType: resType, ObjectId: resID},
		Permission: permission,
		Subject:    &v1.SubjectReference{Object: &v1.ObjectReference{ObjectType: subType, ObjectId: subID}},
		Consistency: &v1.Consistency{
			Requirement: &v1.Consistency_FullyConsistent{FullyConsistent: true},
		},
	})
	if err != nil {
		return false, err
	}
	return resp.Permissionship == v1.CheckPermissionResponse_PERMISSIONSHIP_HAS_PERMISSION, nil
}

// Mock is an in-memory Client for tests.
// Tuples are stored as "resource#relation@subject" strings.
// Mock is goroutine-safe.
type Mock struct {
	mu     sync.RWMutex
	tuples map[string]bool
}

func NewMock() *Mock { return &Mock{tuples: map[string]bool{}} }

func (m *Mock) WriteSchema(_ context.Context, _ string) error { return nil }

func (m *Mock) WriteRelationship(_ context.Context, resource, relation, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tuples[resource+"#"+relation+"@"+subject] = true
	return nil
}

func (m *Mock) DeleteRelationship(_ context.Context, resource, relation, subject string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tuples, resource+"#"+relation+"@"+subject)
	return nil
}

func (m *Mock) DeleteAgentRelationships(_ context.Context, agentID string, resourceTypes []string) error {
	if len(resourceTypes) == 0 {
		return nil
	}
	types := make(map[string]bool, len(resourceTypes))
	for _, rt := range resourceTypes {
		types[rt] = true
	}
	suffix := "@agent:" + agentID
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.tuples {
		// Key shape: "resource#relation@subject" where resource is "type:id".
		// Delete only tuples for this agent whose resource type is in the set,
		// mirroring realClient's per-resource-type filter so tests catch a
		// missing type in the caller's list.
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		resType, _ := parseRef(strings.SplitN(k, "#", 2)[0])
		if types[resType] {
			delete(m.tuples, k)
		}
	}
	return nil
}

// CheckPermission checks whether subject holds permission on resource.
// The mock models the default schema's `work_on = agent & agent->enabled`
// (Phase 5a): permission requires BOTH the "agent" relation tuple AND the
// subject agent's own "enabled" status tuple, mirroring SpiceDB's intersection.
// subject is "agent:<id>", so its enabled tuple key is "<subject>#enabled@<subject>".
func (m *Mock) CheckPermission(_ context.Context, resource, _ /*permission*/ string, subject string) (bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agentKey := resource + "#agent@" + subject
	enabledKey := subject + "#enabled@" + subject
	return m.tuples[agentKey] && m.tuples[enabledKey], nil
}
