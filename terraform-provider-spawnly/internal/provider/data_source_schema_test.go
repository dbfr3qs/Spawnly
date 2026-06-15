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

// newSchemaState builds an empty tfsdk.State carrying the data source's schema,
// ready for Read to populate.
func newSchemaState(t *testing.T, d datasource.DataSource) tfsdk.State {
	t.Helper()
	var sr datasource.SchemaResponse
	d.Schema(context.Background(), datasource.SchemaRequest{}, &sr)
	if sr.Diagnostics.HasError() {
		t.Fatalf("schema build failed: %v", sr.Diagnostics)
	}
	return tfsdk.State{Schema: sr.Schema}
}

func TestSchemaDataSourceRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/schema" {
			http.Error(w, "unexpected path", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema":"definition tenant {}","version":"v7","source":"configmap"}`))
	}))
	defer srv.Close()

	d := &schemaDataSource{client: client.New(srv.URL, "")}
	resp := &datasource.ReadResponse{State: newSchemaState(t, d)}
	d.Read(context.Background(), datasource.ReadRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("unexpected diagnostics: %v", resp.Diagnostics)
	}
	var got schemaModel
	if diags := resp.State.Get(context.Background(), &got); diags.HasError() {
		t.Fatalf("state get failed: %v", diags)
	}
	if got.Schema.ValueString() != "definition tenant {}" {
		t.Errorf("schema = %q, want %q", got.Schema.ValueString(), "definition tenant {}")
	}
	if got.Version.ValueString() != "v7" {
		t.Errorf("version = %q, want %q", got.Version.ValueString(), "v7")
	}
	if got.Source.ValueString() != "configmap" {
		t.Errorf("source = %q, want %q", got.Source.ValueString(), "configmap")
	}
}

func TestSchemaDataSourceReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &schemaDataSource{client: client.New(srv.URL, "")}
	resp := &datasource.ReadResponse{State: newSchemaState(t, d)}
	d.Read(context.Background(), datasource.ReadRequest{}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("expected an error diagnostic on a 500, got none")
	}
}
