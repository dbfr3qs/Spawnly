package provider

import (
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

// fullModel is a templateModel exercising every field, including nested blocks,
// for a toWire -> fromWire round-trip.
func fullModel() templateModel {
	return templateModel{
		AgentType:      types.StringValue("worker"),
		Version:        types.StringValue("1.2.3"),
		Status:         types.StringValue("disabled"),
		RequiresTenant: types.BoolValue(true),
		OAuthScopes:    []types.String{types.StringValue("read"), types.StringValue("write")},
		Meta: &metaModel{
			DisplayName: types.StringValue("Worker"),
			Description: types.StringValue("does work"),
		},
		RuntimeSpec: &runtimeModel{
			Image:        types.StringValue("img:1"),
			Lifecycle:    types.StringValue("long-lived"),
			SupportsChat: types.BoolValue(true),
			EnvDefaults:  map[string]types.String{"K": types.StringValue("V")},
			Resources: &resourcesModel{
				CPULimits:    types.StringValue("500m"),
				MemoryLimits: types.StringValue("256Mi"),
			},
		},
		AuthZ: &authzModel{
			Relations: []relationModel{{
				Resource: types.StringValue("tenant:{{tenant_id}}"),
				Relation: types.StringValue("agent"),
				Subject:  types.StringValue("agent:{{agent_id}}"),
			}},
		},
		Delegation: &delegationModel{
			AllowedChildTypes: []types.String{types.StringValue("child")},
			GrantableScopes:   []types.String{types.StringValue("read")},
			MaxDepth:          types.Int64Value(3),
			ChildPolicies: map[string]childPolicyModel{
				"child": {
					RequireUserConsent: types.BoolValue(true),
					ConsentTTL:         types.StringValue("720h"),
				},
			},
		},
	}
}

func TestRoundTripFull(t *testing.T) {
	in := fullModel()
	got := fromWire(toWire(in))
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("round-trip mismatch:\n got: %#v\nwant: %#v", got, in)
	}
}

// TestFromWireNormalization asserts that empty server-side collections/scalars
// map to null (not empty), an empty status normalizes to "active", and nested
// blocks stay null when the server holds no content for them.
func TestFromWireNormalization(t *testing.T) {
	// A near-empty wire template: only the required identity fields set.
	wire := client.Template{AgentType: "worker", Version: "1"}
	m := fromWire(wire)

	if got := m.Status.ValueString(); got != "active" {
		t.Fatalf("empty status normalized to %q, want active", got)
	}
	if m.OAuthScopes != nil {
		t.Fatalf("empty oauth_scopes -> %v, want nil slice", m.OAuthScopes)
	}
	if m.Meta != nil {
		t.Fatalf("empty meta materialized: %+v, want nil", m.Meta)
	}
	if m.RuntimeSpec != nil {
		t.Fatalf("empty runtime_spec materialized: %+v, want nil", m.RuntimeSpec)
	}
	if m.AuthZ != nil {
		t.Fatalf("empty authz materialized: %+v, want nil", m.AuthZ)
	}
	if m.Delegation != nil {
		t.Fatalf("empty delegation materialized: %+v, want nil", m.Delegation)
	}
}

// TestFromWireEmptyEnvAndScopesNull checks that empty maps/slices inside an
// otherwise-present block normalize to null rather than empty.
func TestFromWireEmptyEnvAndScopesNull(t *testing.T) {
	wire := client.Template{
		AgentType: "worker",
		Version:   "1",
		Runtime:   client.RuntimeSpec{Image: "img:1"}, // present block, empty EnvDefaults
		Delegation: client.DelegationPolicy{
			MaxDepth: 2, // present block, empty slices/maps
		},
	}
	m := fromWire(wire)

	if m.RuntimeSpec == nil {
		t.Fatal("runtime_spec should be present")
	}
	if m.RuntimeSpec.EnvDefaults != nil {
		t.Fatalf("empty env_defaults -> %v, want nil map", m.RuntimeSpec.EnvDefaults)
	}
	if m.RuntimeSpec.Resources != nil {
		t.Fatalf("empty resources materialized: %+v, want nil", m.RuntimeSpec.Resources)
	}
	if m.Delegation == nil {
		t.Fatal("delegation should be present (MaxDepth != 0)")
	}
	if m.Delegation.AllowedChildTypes != nil || m.Delegation.GrantableScopes != nil {
		t.Fatalf("empty delegation slices not null: %+v", m.Delegation)
	}
	if m.Delegation.ChildPolicies != nil {
		t.Fatalf("empty child_policies -> %v, want nil map", m.Delegation.ChildPolicies)
	}
}

// TestToWireEmptyCollectionsNil ensures the model -> wire direction drops empty
// optional collections to nil so omitempty elides them on the wire.
func TestToWireEmptyCollectionsNil(t *testing.T) {
	m := templateModel{
		AgentType:   types.StringValue("worker"),
		Version:     types.StringValue("1"),
		Status:      types.StringValue("active"),
		OAuthScopes: nil,
		Delegation: &delegationModel{
			AllowedChildTypes: nil,
			GrantableScopes:   nil,
			MaxDepth:          types.Int64Value(0),
			ChildPolicies:     nil,
		},
	}
	w := toWire(m)
	if w.OAuthScopes != nil {
		t.Fatalf("OAuthScopes -> %v, want nil", w.OAuthScopes)
	}
	if w.Delegation.AllowedChildTypes != nil || w.Delegation.GrantableScopes != nil {
		t.Fatalf("delegation slices not nil: %+v", w.Delegation)
	}
	if w.Delegation.ChildPolicies != nil {
		t.Fatalf("ChildPolicies -> %v, want nil", w.Delegation.ChildPolicies)
	}
}
