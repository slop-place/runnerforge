// Command terraform-provider-runnerforge manages a runnerforge deployment as
// code: the clouds it provisions machines on, the forges it serves, and the
// pools that connect them.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/slop-place/terraform-provider-runnerforge/internal/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/slop-place/runnerforge",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
