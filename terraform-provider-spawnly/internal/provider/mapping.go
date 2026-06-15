package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

// toWire converts the Terraform model into the registry wire template. Null
// scalars become zero values; null/empty collections become nil so they
// serialize away under the wire type's omitempty tags.
func toWire(m templateModel) client.Template {
	t := client.Template{
		AgentType:      m.AgentType.ValueString(),
		Version:        m.Version.ValueString(),
		Status:         m.Status.ValueString(),
		RequiresTenant: m.RequiresTenant.ValueBool(),
		OAuthScopes:    stringSlice(m.OAuthScopes),
	}

	if m.Meta != nil {
		t.Meta = client.TemplateMeta{
			DisplayName: m.Meta.DisplayName.ValueString(),
			Description: m.Meta.Description.ValueString(),
		}
	}

	if rs := m.RuntimeSpec; rs != nil {
		t.Runtime = client.RuntimeSpec{
			Image:        rs.Image.ValueString(),
			Lifecycle:    rs.Lifecycle.ValueString(),
			SupportsChat: rs.SupportsChat.ValueBool(),
			EnvDefaults:  stringMap(rs.EnvDefaults),
		}
		if rs.Resources != nil {
			t.Runtime.Resources = client.ResourceLimits{
				CPULimit:    rs.Resources.CPULimits.ValueString(),
				MemoryLimit: rs.Resources.MemoryLimits.ValueString(),
			}
		}
	}

	if m.AuthZ != nil {
		for _, rel := range m.AuthZ.Relations {
			t.AuthZ.SpiceDBRelations = append(t.AuthZ.SpiceDBRelations, client.SpiceDBRelation{
				Resource: rel.Resource.ValueString(),
				Relation: rel.Relation.ValueString(),
				Subject:  rel.Subject.ValueString(),
			})
		}
	}

	if d := m.Delegation; d != nil {
		t.Delegation = client.DelegationPolicy{
			AllowedChildTypes: stringSlice(d.AllowedChildTypes),
			GrantableScopes:   stringSlice(d.GrantableScopes),
			MaxDepth:          int(d.MaxDepth.ValueInt64()),
		}
		if len(d.ChildPolicies) > 0 {
			t.Delegation.ChildPolicies = make(map[string]client.ChildSpawnPolicy, len(d.ChildPolicies))
			for k, cp := range d.ChildPolicies {
				t.Delegation.ChildPolicies[k] = client.ChildSpawnPolicy{
					RequireUserConsent: cp.RequireUserConsent.ValueBool(),
					ConsentTTL:         cp.ConsentTTL.ValueString(),
				}
			}
		}
	}

	return t
}

// fromWire converts a registry template into the Terraform model for Read.
// Empty collections map to nil (null) so an omitted-in-config block doesn't show
// a perpetual diff, and an empty server status normalizes to "active".
func fromWire(t client.Template) templateModel {
	status := t.Status
	if status == "" {
		status = "active"
	}
	m := templateModel{
		AgentType:      types.StringValue(t.AgentType),
		Version:        types.StringValue(t.Version),
		Status:         types.StringValue(status),
		RequiresTenant: types.BoolValue(t.RequiresTenant),
		OAuthScopes:    stringValues(t.OAuthScopes),
	}

	// Nested blocks are only materialized when the server holds content for
	// them; an empty block stays null so it matches a config that omitted it
	// (avoids a perpetual null-vs-empty plan diff).
	if t.Meta.DisplayName != "" || t.Meta.Description != "" {
		m.Meta = &metaModel{
			DisplayName: optionalString(t.Meta.DisplayName),
			Description: optionalString(t.Meta.Description),
		}
	}

	if rs := t.Runtime; rs.Image != "" || rs.Lifecycle != "" || rs.SupportsChat ||
		len(rs.EnvDefaults) > 0 || rs.Resources != (client.ResourceLimits{}) {
		m.RuntimeSpec = &runtimeModel{
			Image:        optionalString(rs.Image),
			Lifecycle:    optionalString(rs.Lifecycle),
			SupportsChat: types.BoolValue(rs.SupportsChat),
			EnvDefaults:  stringValueMap(rs.EnvDefaults),
		}
		if rs.Resources != (client.ResourceLimits{}) {
			m.RuntimeSpec.Resources = &resourcesModel{
				CPULimits:    optionalString(rs.Resources.CPULimit),
				MemoryLimits: optionalString(rs.Resources.MemoryLimit),
			}
		}
	}

	if len(t.AuthZ.SpiceDBRelations) > 0 {
		az := &authzModel{}
		for _, rel := range t.AuthZ.SpiceDBRelations {
			az.Relations = append(az.Relations, relationModel{
				Resource: types.StringValue(rel.Resource),
				Relation: types.StringValue(rel.Relation),
				Subject:  types.StringValue(rel.Subject),
			})
		}
		m.AuthZ = az
	}

	if d := t.Delegation; len(d.AllowedChildTypes) > 0 || len(d.GrantableScopes) > 0 ||
		d.MaxDepth != 0 || len(d.ChildPolicies) > 0 {
		m.Delegation = &delegationModel{
			AllowedChildTypes: stringValues(d.AllowedChildTypes),
			GrantableScopes:   stringValues(d.GrantableScopes),
			MaxDepth:          types.Int64Value(int64(d.MaxDepth)),
		}
		if len(d.ChildPolicies) > 0 {
			m.Delegation.ChildPolicies = make(map[string]childPolicyModel, len(d.ChildPolicies))
			for k, cp := range d.ChildPolicies {
				m.Delegation.ChildPolicies[k] = childPolicyModel{
					RequireUserConsent: types.BoolValue(cp.RequireUserConsent),
					ConsentTTL:         optionalString(cp.ConsentTTL),
				}
			}
		}
	}

	return m
}

// --- small conversion helpers ---

func stringSlice(in []types.String) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.ValueString())
	}
	return out
}

func stringValues(in []string) []types.String {
	if len(in) == 0 {
		return nil
	}
	out := make([]types.String, 0, len(in))
	for _, s := range in {
		out = append(out, types.StringValue(s))
	}
	return out
}

func stringMap(in map[string]types.String) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v.ValueString()
	}
	return out
}

func stringValueMap(in map[string]string) map[string]types.String {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]types.String, len(in))
	for k, v := range in {
		out[k] = types.StringValue(v)
	}
	return out
}

// optionalString maps an empty string to null so an omitted optional attribute
// round-trips cleanly.
func optionalString(s string) types.String {
	if s == "" {
		return types.StringNull()
	}
	return types.StringValue(s)
}
