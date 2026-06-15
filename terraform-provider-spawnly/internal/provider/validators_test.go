package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestDurationValidator(t *testing.T) {
	tests := []struct {
		name    string
		value   types.String
		wantErr bool
	}{
		{name: "valid duration", value: types.StringValue("720h"), wantErr: false},
		{name: "empty allowed", value: types.StringValue(""), wantErr: false},
		{name: "null allowed", value: types.StringNull(), wantErr: false},
		{name: "unknown allowed", value: types.StringUnknown(), wantErr: false},
		{name: "invalid -> error", value: types.StringValue("banana"), wantErr: true},
	}

	v := goDurationValidator()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validator.StringRequest{
				Path:        path.Root("consent_ttl"),
				ConfigValue: tc.value,
			}
			resp := &validator.StringResponse{}
			v.ValidateString(context.Background(), req, resp)
			if got := resp.Diagnostics.HasError(); got != tc.wantErr {
				t.Fatalf("HasError = %v, want %v (diags: %v)", got, tc.wantErr, resp.Diagnostics)
			}
		})
	}
}
