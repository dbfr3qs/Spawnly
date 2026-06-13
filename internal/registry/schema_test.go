// internal/registry/schema_test.go
package registry

import "testing"

const defaultTestSchema = `
definition agent {}

definition tenant {
    relation agent: agent
    permission work_on = agent
}
`

func mustParse(t *testing.T, s string) *SchemaModel {
	t.Helper()
	m, err := ParseSchema(s)
	if err != nil {
		t.Fatalf("ParseSchema: %v", err)
	}
	return m
}

func TestParseSchema_DefinitionsAndRelations(t *testing.T) {
	m := mustParse(t, defaultTestSchema)
	if _, ok := m.definitions["agent"]; !ok {
		t.Fatal("expected 'agent' definition")
	}
	tenant, ok := m.definitions["tenant"]
	if !ok {
		t.Fatal("expected 'tenant' definition")
	}
	if _, ok := tenant["agent"]; !ok {
		t.Fatal("expected 'agent' relation on tenant")
	}
	if _, ok := tenant["work_on"]; !ok {
		t.Fatal("expected 'work_on' permission on tenant")
	}
}

func TestParseSchema_EmptyIsError(t *testing.T) {
	if _, err := ParseSchema("// just a comment\n"); err == nil {
		t.Fatal("expected error for schema with no definitions")
	}
}

func TestValidate_AcceptsConformingTemplate(t *testing.T) {
	m := mustParse(t, defaultTestSchema)
	spec := AuthZSpec{SpiceDBRelations: []SpiceDBRelationTemplate{
		{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
	}}
	if err := m.Validate(spec); err != nil {
		t.Fatalf("expected conforming template to validate, got %v", err)
	}
}

func TestValidate_RejectsUnknownResourceDefinition(t *testing.T) {
	m := mustParse(t, defaultTestSchema)
	spec := AuthZSpec{SpiceDBRelations: []SpiceDBRelationTemplate{
		{Resource: "project:{{tenant_id}}", Relation: "agent", Subject: "agent:{{agent_id}}"},
	}}
	if err := m.Validate(spec); err == nil {
		t.Fatal("expected rejection of unknown 'project' resource definition")
	}
}

func TestValidate_RejectsUnknownRelation(t *testing.T) {
	m := mustParse(t, defaultTestSchema)
	spec := AuthZSpec{SpiceDBRelations: []SpiceDBRelationTemplate{
		{Resource: "tenant:{{tenant_id}}", Relation: "contributes_to", Subject: "agent:{{agent_id}}"},
	}}
	if err := m.Validate(spec); err == nil {
		t.Fatal("expected rejection of unknown 'contributes_to' relation on tenant")
	}
}

func TestValidate_RejectsUnknownSubjectDefinition(t *testing.T) {
	m := mustParse(t, defaultTestSchema)
	spec := AuthZSpec{SpiceDBRelations: []SpiceDBRelationTemplate{
		{Resource: "tenant:{{tenant_id}}", Relation: "agent", Subject: "robot:{{agent_id}}"},
	}}
	if err := m.Validate(spec); err == nil {
		t.Fatal("expected rejection of unknown 'robot' subject definition")
	}
}

// TestParseSchema_HandlesIntersectionPermission guards the Phase 5a schema
// shape (permission with an intersection arrow) so validation still parses the
// extended default bundle.
func TestParseSchema_HandlesIntersectionPermission(t *testing.T) {
	m := mustParse(t, `
definition agent {
    relation enabled: agent
}
definition tenant {
    relation agent: agent
    permission work_on = agent & agent->enabled
}
`)
	if _, ok := m.definitions["agent"]["enabled"]; !ok {
		t.Fatal("expected 'enabled' relation on agent")
	}
	if _, ok := m.definitions["tenant"]["work_on"]; !ok {
		t.Fatal("expected 'work_on' permission on tenant")
	}
}
