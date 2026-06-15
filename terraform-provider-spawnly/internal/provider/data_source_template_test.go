package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

// templateReadRequest builds a ReadRequest whose Config carries only the
// required agent_type input, plus a State carrying the data source's schema.
func templateReadRequest(t *testing.T, d datasource.DataSource, agentType string) (datasource.ReadRequest, *datasource.ReadResponse) {
	t.Helper()
	var sr datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &sr)
	if sr.Diagnostics.HasError() {
		t.Fatalf("schema build failed: %v", sr.Diagnostics)
	}

	// tfsdk.Config has no setter, so populate a State (which does) with just the
	// required agent_type and reuse its encoded Raw value for the config.
	cfgState := tfsdk.State{Schema: sr.Schema}
	if diags := cfgState.Set(context.Background(), templateModel{AgentType: types.StringValue(agentType)}); diags.HasError() {
		t.Fatalf("config set failed: %v", diags)
	}
	cfg := tfsdk.Config{Schema: sr.Schema, Raw: cfgState.Raw}

	return datasource.ReadRequest{Config: cfg}, &datasource.ReadResponse{State: tfsdk.State{Schema: sr.Schema}}
}

func TestTemplateDataSourceRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates/pi-worker" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"agentType": "pi-worker",
			"version": "1.2.0",
			"status": "active",
			"requiresTenant": true,
			"oauthScopes": ["compute:read"],
			"meta": {"displayName": "Pi Worker", "description": "computes digits"},
			"runtimeSpec": {
				"image": "ghcr.io/spawnly/pi-worker:1.2.0",
				"lifecycle": "short-lived",
				"supportsChat": false,
				"resources": {"cpuLimits": "500m", "memoryLimits": "256Mi"}
			},
			"authzTemplate": {"spiceDbRelations": [
				{"resource": "tenant:{{tenant_id}}", "relation": "agent", "subject": "agent:{{agent_id}}"}
			]},
			"delegation": {"allowedChildTypes": ["chain-worker"], "maxDepth": 3}
		}`))
	}))
	defer srv.Close()

	d := &templateDataSource{client: client.New(srv.URL, "")}
	req, resp := templateReadRequest(t, d, "pi-worker")
	d.Read(context.Background(), req, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	var got templateModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state get failed: %v", diags)
	}
	if got.AgentType.ValueString() != "pi-worker" {
		t.Errorf("agent_type = %q, want pi-worker", got.AgentType.ValueString())
	}
	if got.Version.ValueString() != "1.2.0" {
		t.Errorf("version = %q, want 1.2.0", got.Version.ValueString())
	}
	if !got.RequiresTenant.ValueBool() {
		t.Error("requires_tenant = false, want true")
	}
	if got.Meta == nil || got.Meta.DisplayName.ValueString() != "Pi Worker" {
		t.Errorf("meta.display_name not mapped: %+v", got.Meta)
	}
	if got.RuntimeSpec == nil || got.RuntimeSpec.Image.ValueString() != "ghcr.io/spawnly/pi-worker:1.2.0" {
		t.Errorf("runtime_spec.image not mapped: %+v", got.RuntimeSpec)
	}
	if got.RuntimeSpec == nil || got.RuntimeSpec.Resources == nil ||
		got.RuntimeSpec.Resources.CPULimits.ValueString() != "500m" {
		t.Errorf("runtime_spec.resources not mapped: %+v", got.RuntimeSpec)
	}
	if got.AuthZ == nil || len(got.AuthZ.Relations) != 1 ||
		got.AuthZ.Relations[0].Relation.ValueString() != "agent" {
		t.Errorf("authz_template not mapped: %+v", got.AuthZ)
	}
	if got.Delegation == nil || got.Delegation.MaxDepth.ValueInt64() != 3 {
		t.Errorf("delegation not mapped: %+v", got.Delegation)
	}
}

func TestTemplateDataSourceReadNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	d := &templateDataSource{client: client.New(srv.URL, "")}
	req, resp := templateReadRequest(t, d, "ghost")
	d.Read(context.Background(), req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected a not-found error diagnostic, got none")
	}
}

func TestTemplateDataSourceReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &templateDataSource{client: client.New(srv.URL, "")}
	req, resp := templateReadRequest(t, d, "pi-worker")
	d.Read(context.Background(), req, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic on a 500, got none")
	}
}
