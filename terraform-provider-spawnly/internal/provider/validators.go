package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// durationValidator rejects a string attribute whose value isn't parseable by
// time.ParseDuration. An empty string is allowed because the registry reads an
// empty consent TTL as "never expires", so a set-but-empty value is meaningful.
type durationValidator struct{}

// goDurationValidator is the constructor used in the resource schema.
func goDurationValidator() validator.String { return durationValidator{} }

func (durationValidator) Description(_ context.Context) string {
	return `value must be a Go duration string (e.g. "720h") or empty`
}

func (v durationValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (durationValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	// Null/unknown can't be validated yet; the framework handles those.
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	s := req.ConfigValue.ValueString()
	if s == "" {
		return
	}
	if _, err := time.ParseDuration(s); err != nil {
		resp.Diagnostics.AddAttributeError(
			req.Path,
			"Invalid duration",
			`value must be a Go duration string (e.g. "720h") or empty; got "`+s+`": `+err.Error(),
		)
	}
}
