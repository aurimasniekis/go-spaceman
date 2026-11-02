// Command spaceman is the SpaceMan central control plane: the embedded web UI, the administrative API, node
// connectivity, orchestration state and operational commands (docs/GOAL.md §1).
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "spaceman:", err)
		os.Exit(1)
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "spaceman",
		Short:         "Self-hosted control plane for persistent teams of coding agents",
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
