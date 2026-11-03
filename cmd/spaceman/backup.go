package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/memory"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// backupCmd holds the backup commands (SM-STORE-003…006, ADR-0031). create, list and verify work while central runs
// (they read the database through a read-only connection and need the KEK); restore needs central stopped.
func backupCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Create, list, verify and restore sealed backups (docs: site/operators/backup.md)"}
	cmd.AddCommand(backupCreateCmd(g), backupListCmd(g), backupVerifyCmd(g), backupRestoreCmd(g))
	return cmd
}

func (g *globals) backupEnv() (*config.Config, backup.Target, *keyring.LocalKEK, error) {
	cfg, err := config.Load(g.config(), os.Environ())
	if err != nil {
		return nil, nil, nil, err
	}
	t, err := backup.TargetFromConfig(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	kek, err := keyring.LoadKEK(keyring.Source{Kind: cfg.KEK.Source, File: cfg.KEK.File, Production: cfg.IsProduction(), DataDir: cfg.DataDir, Environ: os.Environ()})
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, t, kek, nil
}

func artifactsDir(cfg *config.Config) string {
	if cfg.Artifacts.LocalDir != "" {
		return cfg.Artifacts.LocalDir
	}
	return filepath.Join(cfg.DataDir, "artifacts")
}

func gitOrNil() *memory.Git {
	git, err := memory.NewGit()
	if err != nil {
		return nil
	}
	return git
}

func backupCreateCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Write a sealed backup to the configured target (safe while central runs; needs the KEK)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, target, kek, err := g.backupEnv()
			if err != nil {
				return err
			}
			src, err := backup.OpenReadOnly(ctx, cfg.DataDir)
			if err != nil {
				return err
			}
			defer src.Close()
			cfgYAML, _ := yaml.Marshal(cfg)
			if err := os.MkdirAll(target.StagingDir(), 0o700); err != nil {
				return err
			}
			id := ids.New(ids.Backup)
			dest := filepath.Join(target.StagingDir(), id)
			sources := backup.DefaultSources(cfg.DataDir, cfg.Artifacts.LocalDir, gitOrNil())
			if cfg.Artifacts.Backend == "s3" {
				// Artifacts in the S3 artifact store are copied into the backup (ADR-0048).
				if sources.Store, err = artifacts.S3FromConfig(cfg); err != nil {
					return err
				}
			}
			res, err := backup.Create(ctx, src, kek, dest, backup.Options{ID: id, Sources: sources, Config: cfgYAML, MemoryFlush: backup.OfflineMemoryFlush()})
			if err != nil {
				return err
			}
			if target.Kind() != "local" {
				defer func() { _ = os.RemoveAll(dest) }()
			}
			if err := target.Store(ctx, dest); err != nil {
				return err
			}
			m := res.Manifest
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(m)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "backup %s written to %s\n", m.ID, target.Location())
			fmt.Fprintf(w, "  schema version %d, %d files, %d bytes (%d sealed)\n", m.SchemaVersion, len(m.Files), m.Bytes, m.SealedBytes)
			fmt.Fprintf(w, "  %d spaces, %d runs, %d open human actions, %d pending approvals, %d subscriptions, %d memory collections\n",
				m.Counts.Spaces, m.Counts.Runs, m.Counts.OpenHumanActions, m.Counts.PendingApprovals, m.Counts.Subscriptions, m.Counts.MemoryCollections)
			fmt.Fprintf(w, "  sealed under KEK %s: the KEK is NOT in the backup; keep it backed up separately\n", m.KEKID)
			for _, s := range m.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", s)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the manifest as JSON")
	return cmd
}

func backupListCmd(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the backups in the configured target, newest first",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(g.config(), os.Environ())
			if err != nil {
				return err
			}
			target, err := backup.TargetFromConfig(cfg)
			if err != nil {
				return err
			}
			es, err := target.List(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(es)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tCREATED\tFILES\tSTORED BYTES\tSTATE")
			for _, e := range es {
				state, files := "complete", "-"
				if !e.Complete {
					state = "incomplete"
				} else if e.Manifest != nil {
					files = fmt.Sprint(len(e.Manifest.Files))
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n", e.ID, e.CreatedAt.Format(time.RFC3339), files, e.StoredBytes, state)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// fetch resolves ID-or-directory: an existing directory is used as is; otherwise the ID is fetched from the target.
func fetchBackup(ctx context.Context, target backup.Target, arg, tmpRoot string) (string, func(), error) {
	if fi, err := os.Stat(arg); err == nil && fi.IsDir() {
		return arg, func() {}, nil
	}
	if !backup.ValidID(arg) {
		return "", nil, fmt.Errorf("%s is neither a backup directory nor a backup ID (bkp_…)", arg)
	}
	return target.Fetch(ctx, arg, tmpRoot)
}

func backupVerifyCmd(g *globals) *cobra.Command {
	var asJSON, noKey bool
	cmd := &cobra.Command{
		Use:   "verify ID|DIR",
		Short: "Verify a backup: checksums, sealed objects opened with the KEK, database integrity, audit chain",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(g.config(), os.Environ())
			if err != nil {
				return err
			}
			target, err := backup.TargetFromConfig(cfg)
			if err != nil {
				return err
			}
			var kek keyring.KeyWrapper
			if !noKey {
				k, err := keyring.LoadKEK(keyring.Source{Kind: cfg.KEK.Source, File: cfg.KEK.File, Production: cfg.IsProduction(), DataDir: cfg.DataDir, Environ: os.Environ()})
				if err != nil {
					return fmt.Errorf("%w (use --no-key to check only what needs no key)", err)
				}
				kek = k
			}
			tmp, err := os.MkdirTemp("", "spaceman-verify-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(tmp)
			dir, cleanup, err := fetchBackup(ctx, target, args[0], tmp)
			if err != nil {
				return err
			}
			defer cleanup()
			rep, verr := backup.Verify(ctx, dir, backup.VerifyOptions{KEK: kek, TempDir: tmp})
			if asJSON {
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(rep); err != nil {
					return err
				}
			} else {
				printReport(cmd.OutOrStdout(), rep)
			}
			return verr
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.Flags().BoolVar(&noKey, "no-key", false, "check only the checksums of the sealed objects (no KEK needed)")
	return cmd
}

func printReport(w io.Writer, rep *backup.Report) {
	if m := rep.Manifest; m != nil {
		fmt.Fprintf(w, "backup %s created %s by spaceman %s, schema %d, KEK %s\n", m.ID, m.CreatedAt.Format(time.RFC3339), m.SpaceManVersion, m.SchemaVersion, m.KEKID)
	}
	if rep.Keyed {
		fmt.Fprintf(w, "files: %d opened and checked; audit chain: %d entries link correctly\n", rep.Files, rep.AuditEvents)
	} else {
		fmt.Fprintf(w, "files: %d sealed objects match their checksums (contents not opened: no KEK)\n", rep.Files)
	}
	for _, s := range rep.Warnings {
		fmt.Fprintf(w, "warning: %s\n", s)
	}
	for _, s := range rep.Problems {
		fmt.Fprintf(w, "PROBLEM: %s\n", s)
	}
	if rep.OK {
		fmt.Fprintln(w, "backup: ok")
	} else {
		fmt.Fprintln(w, "backup: FAILED")
	}
}

func backupRestoreCmd(g *globals) *cobra.Command {
	var dataDir string
	var force bool
	cmd := &cobra.Command{
		Use:   "restore ID|DIR",
		Short: "Restore a backup into an empty data directory (offline: central must be stopped; needs the KEK)",
		Long: "Restores the database, role memory, recordings, attachments and transcripts. The first start afterwards pauses every space, " +
			"renews execution authority (lease generations), marks active runs unknown, replays event subscriptions from their oldest stored " +
			"delivery and revokes approvals with uses left. Resume each space explicitly once its node has reconciled.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(g.config(), os.Environ())
			if err != nil {
				return err
			}
			if dataDir != "" {
				if dataDir, err = filepath.Abs(dataDir); err != nil {
					return err
				}
				cfg.DataDir = dataDir
			}
			if cfg.KEK.Source != "env" && cfg.KEK.File == "" {
				return errors.New("restore needs the KEK the backup was sealed under: set kek.file (or kek.source=env with SPACEMAN_KEK); " +
					"a development KEK inside a data directory is never part of a backup, so copy it to a file and point kek.file at it")
			}
			kek, err := keyring.LoadKEK(keyring.Source{Kind: cfg.KEK.Source, File: cfg.KEK.File, Production: true, Environ: os.Environ()})
			if err != nil {
				return err
			}
			target, err := backup.TargetFromConfig(cfg)
			if err != nil {
				return err
			}
			tmpRoot := filepath.Join(cfg.DataDir, store.BackupsDir)
			src, cleanup, err := fetchBackup(ctx, target, args[0], tmpRoot)
			if err != nil {
				return err
			}
			defer cleanup()
			allowNetFS := !cfg.IsProduction() && os.Getenv("SPACEMAN_ALLOW_NETWORK_FS") == "1"
			res, err := backup.Restore(ctx, src, backup.RestoreOptions{DataDir: cfg.DataDir, ArtifactsDir: artifactsDir(cfg), KEK: kek, Force: force,
				Git: gitOrNil(), AllowNetworkFS: allowNetFS})
			if err != nil {
				return err
			}
			for _, w := range res.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			// Prove that the KEK unlocks the restored keyring (this also applies pending migrations) before saying "start".
			st, err := store.Open(ctx, store.Options{DataDir: cfg.DataDir, AllowNetworkFS: allowNetFS})
			if err != nil {
				return fmt.Errorf("restored into %s, but the database does not open: %w", res.DataDir, err)
			}
			_, kerr := keyring.Load(ctx, st, kek)
			_ = st.Close()
			if kerr != nil {
				return fmt.Errorf("restored into %s, but the KEK does not unlock its keyring: %w", res.DataDir, kerr)
			}
			m := res.Manifest
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "restored backup %s (created %s) into %s: %d files, %d bytes; KEK %s unlocks the keyring\n",
				m.ID, m.CreatedAt.Format(time.RFC3339), res.DataDir, res.Files, res.Bytes, kek.ID())
			if res.ConfigCopy != "" {
				fmt.Fprintf(w, "  the backup's configuration is at %s for comparison (it is not applied)\n", res.ConfigCopy)
			}
			fmt.Fprintln(w, "next:")
			if cfg.Artifacts.Backend == "s3" {
				fmt.Fprintln(w, "  0. artifacts.backend is s3, and the backup's artifacts were restored to the local folders: upload them first with")
				fmt.Fprintln(w, "     spaceman artifacts migrate --to s3 --remove-source")
			}
			fmt.Fprintln(w, "  1. start central (spaceman serve): every space comes up paused and execution authority is renewed")
			fmt.Fprintln(w, "  2. let nodes reconnect and reconcile; runs from before the restore are stopped (fenced)")
			fmt.Fprintln(w, "  3. check the restore notice (Backups page or GET /api/v1/system/restore), then resume each space explicitly")
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data", "", "data directory to restore into (default: data_dir from the configuration)")
	cmd.Flags().BoolVar(&force, "force", false, "move existing data aside (to backups/pre-restore-*) instead of refusing a non-empty target")
	return cmd
}
