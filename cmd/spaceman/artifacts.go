package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// artifactsCmd holds the offline artifact-store commands (SM-STORE-002, ADR-0048).
func artifactsCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "artifacts", Short: "Move stored attachments, recordings and transcripts between the local disk and S3 (docs: site/operators/storage.md)"}
	cmd.AddCommand(artifactsMigrateCmd(g))
	return cmd
}

func artifactsMigrateCmd(g *globals) *cobra.Command {
	var to string
	var removeSource, dryRun, asJSON bool
	cmd := &cobra.Command{
		Use:   "migrate --to s3|local",
		Short: "Copy every stored artifact to the S3 bucket (artifacts.s3) or back to the local folders (offline: central must be stopped)",
		Long: "Copies attachment content (its SHA-256 checked), recording segments and transcripts that the destination lacks, " +
			"then optionally removes them from the source. It takes the data-directory lock, so central must be stopped, and it is " +
			"safe to repeat after an interruption. Switch artifacts.backend afterwards and start central. Run it with --to s3 after " +
			"restoring a backup into an installation whose artifacts.backend is s3: a restore writes artifacts to the local folders.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if to != "s3" && to != "local" {
				return errors.New("--to must be s3 or local")
			}
			cfg, err := config.Load(g.config(), os.Environ())
			if err != nil {
				return err
			}
			if cfg.DataDir, err = filepath.Abs(cfg.DataDir); err != nil {
				return err
			}
			lock, err := store.AcquireLock(filepath.Join(cfg.DataDir, store.LockFile))
			if err != nil {
				return fmt.Errorf("%w (stop central first: artifacts migrate needs the data directory to itself)", err)
			}
			defer func() { _ = lock.Release() }()
			local := artifacts.LocalFromConfig(cfg)
			remote, err := artifacts.S3FromConfig(cfg)
			if err != nil {
				return err
			}
			var src, dst artifacts.Store = local, remote
			if to == "local" {
				src, dst = remote, local
			}
			tmp, err := os.MkdirTemp(cfg.DataDir, ".artifacts-migrate-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(tmp) }()
			rep, err := artifacts.Migrate(ctx, src, dst, artifacts.MigrateOptions{TmpDir: tmp, RemoveSource: removeSource, DryRun: dryRun})
			if asJSON && rep != nil {
				if jerr := json.NewEncoder(cmd.OutOrStdout()).Encode(rep); jerr != nil {
					return jerr
				}
			}
			if err != nil {
				if rep != nil && !asJSON {
					fmt.Fprintf(cmd.ErrOrStderr(), "stopped after copying %d artifacts (%d bytes); run the command again to continue\n", rep.Copied, rep.Bytes)
				}
				return err
			}
			if asJSON {
				return nil
			}
			w := cmd.OutOrStdout()
			verb := "copied"
			if dryRun {
				verb = "would copy"
			}
			fmt.Fprintf(w, "from %s\nto   %s\n%s %d artifacts (%d bytes); %d were already there", rep.From, rep.To, verb, rep.Copied, rep.Bytes, rep.Present)
			if removeSource && !dryRun {
				fmt.Fprintf(w, "; removed %d from the source", rep.Removed)
			}
			fmt.Fprintln(w)
			if !dryRun && cfg.Artifacts.Backend != to {
				fmt.Fprintf(w, "next: set artifacts.backend: %s in the configuration, then start central\n", to)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "destination: s3 (the artifacts.s3 bucket) or local (the local folders)")
	cmd.Flags().BoolVar(&removeSource, "remove-source", false, "remove each artifact from the source once the destination holds it")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "only count what would be copied")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}
