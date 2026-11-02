// Command spaceman-agent is the SpaceMan node daemon. It connects outbound to central, keeps a local durable
// execution journal, supervises runs through herdr-core and enforces execution leases (docs/GOAL.md §6).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "spaceman-agent:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "spaceman-agent",
		Short:         "SpaceMan node agent",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(versionCmd())
	return root
}

func versionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return buildinfo.Print(cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}
