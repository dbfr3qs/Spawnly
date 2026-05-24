package spicedb

import (
	"context"
	"fmt"
	"strings"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	authzed "github.com/authzed/authzed-go/v1"
	"github.com/authzed/grpcutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Client is the interface used by operator, sample-api, and orchestrator.
// resource and subject are SpiceDB object strings: "type:id".
type Client interface {
	WriteSchema(ctx context.Context, schema string) error
	// WriteRelationship writes a single tuple: resource#relation@subject.
	WriteRelationship(ctx context.Context, resource, relation, subject string) error
	// DeleteAgentRelationships removes every tuple whose subject is "agent:agentID".
	DeleteAgentRelationships(ctx context.Context, agentID string) error
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

func (r *realClient) DeleteAgentRelationships(ctx context.Context, agentID string) error {
	_, err := r.c.DeleteRelationships(ctx, &v1.DeleteRelationshipsRequest{
		RelationshipFilter: &v1.RelationshipFilter{
			ResourceType: "tenant",
			OptionalSubjectFilter: &v1.SubjectFilter{
				SubjectType:       "agent",
				OptionalSubjectId: agentID,
			},
		},
	})
	return err
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
type Mock struct {
	tuples map[string]bool
}

func NewMock() *Mock { return &Mock{tuples: map[string]bool{}} }

func (m *Mock) WriteSchema(_ context.Context, _ string) error { return nil }

func (m *Mock) WriteRelationship(_ context.Context, resource, relation, subject string) error {
	m.tuples[resource+"#"+relation+"@"+subject] = true
	return nil
}

func (m *Mock) DeleteAgentRelationships(_ context.Context, agentID string) error {
	suffix := "@agent:" + agentID
	for k := range m.tuples {
		if strings.HasSuffix(k, suffix) {
			delete(m.tuples, k)
		}
	}
	return nil
}

// CheckPermission in the mock checks if any tuple grants the subject the
// permission via direct relation membership (sufficient for POC tests).
func (m *Mock) CheckPermission(_ context.Context, resource, permission, subject string) (bool, error) {
	// For the POC schema, work_on is satisfied by the "agent" relation.
	key := resource + "#agent@" + subject
	return m.tuples[key], nil
}
