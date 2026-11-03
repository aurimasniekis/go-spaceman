// Command spaceman-agent is the SpaceMan node daemon. It connects outbound to central, keeps a local durable
// execution journal, supervises runs through herdr-core and enforces execution leases (docs/GOAL.md §6).
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/runner/hc"
)

func main() {
	if err := newRoot().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "spaceman-agent:", err)
		os.Exit(1)
	}
}

type globals struct{ configPath string }

func newRoot() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{Use: "spaceman-agent", Short: "SpaceMan node agent", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVarP(&g.configPath, "config", "c", os.Getenv("SPACEMAN_AGENT_CONFIG"), "configuration file (env SPACEMAN_AGENT_CONFIG)")
	root.AddCommand(versionCmd(), enrollCmd(g), runCmd(g), launchCmd(), leaseGuardCmd(), mcpBridgeCmd())
	return root
}

func versionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{Use: "version", Short: "Print build information", RunE: func(cmd *cobra.Command, _ []string) error {
		return buildinfo.Print(cmd.OutOrStdout(), asJSON)
	}}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func (g *globals) load() (*config.AgentConfig, *slog.Logger, error) {
	cfg, err := config.LoadAgent(g.configPath, os.Environ())
	if err != nil {
		return nil, nil, err
	}
	log, _ := obs.NewLogger(os.Stderr, cfg.Log)
	return cfg, log.With(slog.String(obs.FieldServiceName, "spaceman-agent")), nil
}

func httpClient(cfg *config.AgentConfig) (*http.Client, error) {
	if cfg.Central.CAFile == "" {
		return &http.Client{Timeout: 0}, nil
	}
	pem, err := os.ReadFile(cfg.Central.CAFile)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("central.ca_file: no certificates")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}}}, nil
}

func enrollCmd(g *globals) *cobra.Command {
	var token string
	var force bool
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Exchange a one-time enrollment token for this node's credential (written 0600 to central.credential_file)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _, err := g.load()
			if err != nil {
				return err
			}
			if token == "" {
				token = strings.TrimSpace(os.Getenv("SPACEMAN_ENROLL_TOKEN"))
			}
			if token == "" {
				return errors.New("--token (or SPACEMAN_ENROLL_TOKEN) is required")
			}
			if _, err := os.Stat(cfg.Central.CredentialFile); err == nil && !force {
				return fmt.Errorf("%s exists; pass --force to replace this node's credential", cfg.Central.CredentialFile)
			}
			hcl, err := httpClient(cfg)
			if err != nil {
				return err
			}
			body, _ := json.Marshal(map[string]string{"token": token})
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(cfg.Central.URL, "/")+fleet.EnrollPath, bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			res, err := hcl.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<16))
			if res.StatusCode != http.StatusOK {
				return fmt.Errorf("enrollment refused: %s", res.Status)
			}
			var er fleet.EnrollResult
			if err := json.Unmarshal(b, &er); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(cfg.Central.CredentialFile), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(cfg.Central.CredentialFile, []byte(er.Credential+"\n"), 0o600); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enrolled as node %s (%s). Credential written to %s.\n", er.NodeName, er.NodeID, cfg.Central.CredentialFile)
			return nil
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "enrollment token (spaceman_en_…)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing credential")
	return cmd
}

func runCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the node agent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, log, err := g.load()
			if err != nil {
				return err
			}
			cred, err := os.ReadFile(cfg.Central.CredentialFile)
			if err != nil {
				return fmt.Errorf("read node credential (run `spaceman-agent enroll` first): %w", err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			self, err := os.Executable()
			if err != nil {
				return err
			}
			hcBin := cfg.Runner.HCBinary
			client, err := hc.EnsureCore(ctx, hc.CoreOptions{Binary: hcBin, Session: cfg.Runner.Session, Socket: cfg.Runner.Socket, Home: filepath.Join(cfg.DataDir, "hc"),
				TerminalSize: cfg.Runner.TerminalSize})
			if err != nil {
				return fmt.Errorf("herdr-core: %w", err)
			}
			rn := hc.New(client)
			defer rn.Close()
			j, err := agent.OpenJournal(ctx, filepath.Join(cfg.DataDir, "journal"), obs.Component(log, "journal"), nil)
			if err != nil {
				return err
			}
			defer j.Close()
			hcl, err := httpClient(cfg)
			if err != nil {
				return err
			}
			a := agent.New(j, agent.Options{
				DataDir: cfg.DataDir, CentralURL: cfg.Central.URL, Credential: strings.TrimSpace(string(cred)), HTTPClient: hcl, Runner: rn,
				RunnerInfo:      proto.RunnerInfo{Kind: "herdr-core", Version: client.Hello().Version, CoreID: client.Hello().Core.ID},
				HarnessBinaries: map[string]string{harness.KindClaude: cfg.Harness.ClaudeBinary, harness.KindCodex: cfg.Harness.CodexBinary},
				AgentBinary:     self, BridgeSocket: filepath.Join(filepath.Dir(cfg.Runner.Socket), "bridge.sock"), Capacity: cfg.Capacity,
				ReconnectMax: cfg.Central.ReconnectMax.D(), Log: log,
			})
			log.Info("spaceman-agent starting", slog.String("central", cfg.Central.URL), slog.String("hc_core", client.Hello().Core.ID),
				slog.String("hc_version", client.Hello().Version))
			return a.Run(ctx)
		},
	}
}

func launchCmd() *cobra.Command {
	var secrets string
	cmd := &cobra.Command{
		Use:    "launch --secrets-file FILE -- COMMAND [ARGS…]",
		Short:  "Internal: launcher shim (reads and unlinks the run's secret file, then execs the harness)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return agent.LaunchShim(secrets, args)
		},
	}
	cmd.Flags().StringVar(&secrets, "secrets-file", "", "per-run secret file")
	return cmd
}

func leaseGuardCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:    "lease-guard --lease-file FILE",
		Short:  "Internal: parent terminal of a space's runs; exits when the lease file expires (ADR-0017)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM)
			defer stop()
			err := agent.LeaseGuard(ctx, file, time.Now)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "lease-file", "", "lease file written by the agent")
	return cmd
}

func mcpBridgeCmd() *cobra.Command {
	var run, socket string
	cmd := &cobra.Command{
		Use:    "mcp-bridge --run RUN --socket SOCKET",
		Short:  "Internal: stdio MCP server that forwards the harness's SpaceMan tool calls to the node agent",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return agent.MCPBridge(cmd.Context(), os.Stdin, os.Stdout, run, socket)
		},
	}
	cmd.Flags().StringVar(&run, "run", "", "run ID")
	cmd.Flags().StringVar(&socket, "socket", "", "agent bridge socket")
	return cmd
}
