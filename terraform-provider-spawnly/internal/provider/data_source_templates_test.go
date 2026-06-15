package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"

	"github.com/spawnly/terraform-provider-spawnly/internal/client"
)

func newTemplatesState(t *testing.T, d datasource.DataSource) tfsdk.State {
	t.Helper()
	var sr datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &sr)
	if sr.Diagnostics.HasError() {
		t.Fatalf("schema build failed: %v", sr.Diagnostics)
	}
	return tfsdk.State{Schema: sr.Schema}
}

func TestTemplatesDataSourceRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/templates" {
			http.Error(w, "unexpected path", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`["pi-worker","chain-worker","flue"]`))
	}))
	defer srv.Close()

	d := &templatesDataSource{client: client.New(srv.URL, "")}
	resp := &datasource.ReadResponse{State: newTemplatesState(t, d)}
	d.Read(context.Background(), datasource.ReadRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	var got templatesModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state get failed: %v", diags)
	}
	want := []string{"pi-worker", "chain-worker", "flue"}
	if len(got.AgentTypes) != len(want) {
		t.Fatalf("agent_types len = %d, want %d (%v)", len(got.AgentTypes), len(want), got.AgentTypes)
	}
	for i, w := range want {
		if got.AgentTypes[i].ValueString() != w {
			t.Errorf("agent_types[%d] = %q, want %q", i, got.AgentTypes[i].ValueString(), w)
		}
	}
}

func TestTemplatesDataSourceReadEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	d := &templatesDataSource{client: client.New(srv.URL, "")}
	resp := &datasource.ReadResponse{State: newTemplatesState(t, d)}
	d.Read(context.Background(), datasource.ReadRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	var got templatesModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state get failed: %v", diags)
	}
	if len(got.AgentTypes) != 0 {
		t.Errorf("expected empty agent_types, got %v", got.AgentTypes)
	}
}

func TestTemplatesDataSourceReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &templatesDataSource{client: client.New(srv.URL, "")}
	resp := &datasource.ReadResponse{State: newTemplatesState(t, d)}
	d.Read(context.Background(), datasource.ReadRequest{}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic on a 500, got none")
	}
}
