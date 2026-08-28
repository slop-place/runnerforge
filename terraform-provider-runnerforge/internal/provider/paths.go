package provider

import tfpath "github.com/hashicorp/terraform-plugin-framework/path"

// path is a shorthand for a root attribute path, which appears often enough in
// diagnostics that the full package name is noise.
func path(name string) tfpath.Path { return tfpath.Root(name) }
