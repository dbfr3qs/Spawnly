// terraform-provider-spawnly serves the Spawnly Terraform provider, which
// manages agent templates in the Spawnly registry's control plane.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/spawnly/terraform-provider-spawnly/internal/provider"
)

// version is overridden at build time via -ldflags. It is surfaced in the
// provider's metadata for diagnostics.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		// The source address consumers reference in required_providers. Under a
		// local dev_overrides install this string is matched verbatim; no
		// network registry lookup happens.
		Address: "registry.terraform.io/spawnly/spawnly",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
