package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// mapToAny converts a Terraform string map into the shape the API takes.
//
// Everything is carried as a string: HCL maps are single-typed, and runnerforge
// accepts strings for every setting because that is what an HTML form submits
// too. The driver interprets them.
func mapToAny(ctx context.Context, m types.Map) (map[string]any, diag.Diagnostics) {
	if m.IsNull() || m.IsUnknown() {
		return nil, nil
	}
	var raw map[string]string
	diags := m.ElementsAs(ctx, &raw, false)
	if diags.HasError() {
		return nil, diags
	}
	out := make(map[string]any, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	return out, diags
}

// listToStrings converts a Terraform list into a plain slice.
func listToStrings(ctx context.Context, l types.List) ([]string, diag.Diagnostics) {
	if l.IsNull() || l.IsUnknown() {
		return nil, nil
	}
	var out []string
	diags := l.ElementsAs(ctx, &out, false)
	return out, diags
}
