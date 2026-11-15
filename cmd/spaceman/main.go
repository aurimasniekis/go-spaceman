// Command spaceman is the SpaceMan central control plane: the embedded web UI, the administrative API, node
// connectivity, orchestration state and operational commands (docs/GOAL.md §1).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/cli"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/keyring"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	webui "github.com/aurimasniekis/go-spaceman/web"
)

func main() {
	if err := newRoot(nil).Execute(); err != nil {
		cli.PrintError(os.Stderr, err)
		os.Exit(cli.ExitCode(err))
	}
}

type globals struct{ configPath string }

// config returns --config, else SPACEMAN_CONFIG.
func (g *globals) config() string {
	if g.configPath != "" {
		return g.configPath
	}
	return os.Getenv("SPACEMAN_CONFIG")
}

// newRoot builds the command tree; remote (nil in production) configures the remote client (tests inject transports).
func newRoot(remote *cli.App) *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "spaceman",
		Short: "Self-hosted control plane for persistent teams of coding agents",
		Long: "spaceman runs the SpaceMan control plane (serve) and its offline administration (admin, keys, backup), and is a\n" +
			"remote client for a running central's administrative API (--server and an API token).",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&g.configPath, "config", "c", "", "configuration file of serve and the offline commands (env SPACEMAN_CONFIG)")
	if remote == nil {
		remote = &cli.App{}
	}
	if remote.SpecSource == nil {
		remote.SpecSource = specDocument
	}
	if remote.UserAgent == "" {
		remote.UserAgent = "spaceman-cli/" + buildinfo.Version
	}
	remote.BindFlags(root)
	root.AddGroup(&cobra.Group{ID: "local", Title: "Control plane and offline administration:"},
		&cobra.Group{ID: cli.GroupID, Title: "Remote commands (a running central's API; --server and an API token):"})
	for _, c := range []*cobra.Command{versionCmd(), serveCmd(g), adminCmd(g), keysCmd(), openapiCmd(), backupCmd(g), artifactsCmd(g), docsCmd(root)} {
		c.GroupID = "local"
		root.AddCommand(c)
	}
	root.AddCommand(remote.Commands()...)
	return root
}

// specDocument is the OpenAPI document of this build, which binds every remote command to its operation.
func specDocument() ([]byte, error) {
	a := api.Register(http.NewServeMux(), app.SpecDeps())
	return json.Marshal(a.OpenAPI())
}

func docsCmd(root *cobra.Command) *cobra.Command {
	cmd := &cobra.Command{Use: "docs", Short: "Generate documentation"}
	var out string
	gen := &cobra.Command{
		Use:   "cli",
		Short: "Write the command-line reference (Markdown for the site) into a directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			return cli.WriteDocs(root, out)
		},
	}
	gen.Flags().StringVar(&out, "out", "", "output directory (site/reference/cli)")
	cmd.AddCommand(gen)
	return cmd
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

func (g *globals) load() (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(g.config(), os.Environ())
	if err != nil {
		return nil, nil, err
	}
	log, _ := obs.NewLogger(os.Stderr, cfg.Log)
	return cfg, log, nil
}

func serveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the control plane (API, web UI, node connections, management listener)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, log, err := g.load()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, err := app.OpenCentral(ctx, cfg, log, app.Options{})
			if err != nil {
				return err
			}
			defer closeCentral(c)
			if dist, ok := webui.Dist(); ok {
				c.UI = webui.Handler(dist)
			}
			if on, at, err := c.Auth.BreakGlassAlert(ctx); err == nil && on {
				log.Warn("break-glass recovery was used and is not acknowledged", slog.Time("at", at))
			}
			return c.Serve(ctx)
		},
	}
}

// closeCentral closes central and logs a failure to close (a store connection still in use at the shutdown deadline,
// say), which a deferred Close would drop.
func closeCentral(c *app.Central) {
	if err := c.Close(); err != nil {
		if c.Log != nil {
			c.Log.Error("closing central failed", obs.Err(err))
		} else {
			fmt.Fprintln(os.Stderr, "spaceman: closing central failed:", err)
		}
	}
}

// offline opens the data directory exclusively for an administrative command (the server must be stopped).
func (g *globals) offline(ctx context.Context) (*app.Central, error) {
	cfg, log, err := g.load()
	if err != nil {
		return nil, err
	}
	return app.OpenCentral(ctx, cfg, log, app.Options{})
}

func adminCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Bootstrap and break-glass administration (offline: takes the data-directory lock; needs the KEK)"}
	cmd.AddCommand(&cobra.Command{
		Use:   "bootstrap",
		Short: "Issue the root token once; use it to create the first administrator in the web UI (/setup) or API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.offline(cmd.Context())
			if err != nil {
				return err
			}
			defer closeCentral(c)
			tok, rt, err := c.Auth.IssueRootToken(cmd.Context())
			if err != nil {
				return err
			}
			return printSecret(cmd.OutOrStdout(), "root token", tok, rt.ID)
		},
	})
	rootTok := &cobra.Command{Use: "root-token", Short: "Manage the bootstrap root token"}
	rootTok.AddCommand(&cobra.Command{
		Use:   "rotate",
		Short: "Revoke the active root token (if any) and issue a new one",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := g.offline(cmd.Context())
			if err != nil {
				return err
			}
			defer closeCentral(c)
			tok, rt, err := c.Auth.RotateRootToken(cmd.Context(), audit.Actor{Type: audit.ActorCLI, ID: "admin"})
			if err != nil {
				return err
			}
			return printSecret(cmd.OutOrStdout(), "root token", tok, rt.ID)
		},
	})
	cmd.AddCommand(rootTok)
	var newAdmin string
	recover := &cobra.Command{
		Use:   "recover [user-id]",
		Short: "Break-glass: issue a one-time recovery login for a user, or create a new administrator (--new-admin)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 0) == (newAdmin == "") {
				return fmt.Errorf("give exactly one of a user ID or --new-admin EMAIL")
			}
			c, err := g.offline(cmd.Context())
			if err != nil {
				return err
			}
			defer closeCentral(c)
			user := ""
			if len(args) == 1 {
				user = args[0]
			}
			tok, exp, err := c.Auth.IssueRecoveryLogin(cmd.Context(), user, newAdmin)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recovery login token (single use, expires %s):\n\n  %s\n\nOpen <public_url>/recover and paste it, or POST it to /api/v1/auth/recover. This use is audited and raises an alert until acknowledged.\n",
				exp.Format("2006-01-02 15:04 MST"), tok)
			return nil
		},
	}
	recover.Flags().StringVar(&newAdmin, "new-admin", "", "create a new administrator with this email instead")
	cmd.AddCommand(recover)
	return cmd
}

func printSecret(w io.Writer, what, secret, id string) error {
	_, err := fmt.Fprintf(w, "New %s (%s). It is shown only once; store it offline.\n\n  %s\n\n"+
		"Use it to create the first administrator (web UI /setup, or POST /api/v1/users with Authorization: Bearer).\n"+
		"Revoke it afterwards on the Account page or with DELETE /api/v1/admin/root-token.\n", what, id, secret)
	return err
}

func keysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Key-encryption key management"}
	var out string
	initKEK := &cobra.Command{
		Use:   "init-kek",
		Short: "Generate a new key-encryption key file (mode 0400, never overwrites)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return fmt.Errorf("--out is required")
			}
			k, err := keyring.GenerateKEK()
			if err != nil {
				return err
			}
			if err := keyring.WriteKEKFile(out, k); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s (KEK id %s). Back it up separately from data backups: without it, encrypted data is unrecoverable.\n",
				out, keyring.KEKID(k))
			return nil
		},
	}
	initKEK.Flags().StringVar(&out, "out", "", "output file")
	cmd.AddCommand(initKEK)
	return cmd
}

func openapiCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "openapi",
		Short: "Print the OpenAPI 3.1 document of the API",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := api.Register(http.NewServeMux(), app.SpecDeps())
			b, err := json.MarshalIndent(a.OpenAPI(), "", "  ")
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(append(b, '\n'))
			return err
		},
	}
}
