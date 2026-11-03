// Command terraform-provider-spaceman is the SpaceMan Terraform provider (a nested module of the SpaceMan repository).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/aurimasniekis/go-spaceman/terraform-provider-spaceman/internal/provider"
)

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/aurimasniekis/spaceman",
		Debug:   *debug,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
