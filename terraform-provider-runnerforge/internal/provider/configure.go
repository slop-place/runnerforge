package provider

import (
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// clientFrom pulls the shared client out of a Configure request.
//
// Terraform calls Configure with nil provider data during validation, before
// the provider block is resolved, so that case returns quietly rather than
// reporting an error the operator cannot act on.
func clientFrom(req resource.ConfigureRequest, resp *resource.ConfigureResponse) *Client {
	if req.ProviderData == nil {
		return nil
	}
	client, ok := req.ProviderData.(*Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			"The provider did not supply a runnerforge client. This is a bug in the provider.")
		return nil
	}
	return client
}
