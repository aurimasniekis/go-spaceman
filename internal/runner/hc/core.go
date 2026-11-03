package hc

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CoreOptions locate the node agent's own herdr-core core. SpaceMan never uses the operator's default core (S-0).
type CoreOptions struct {
	Binary  string // hc
	Session string // dedicated session name, never "default"
	// Socket is the core socket; it must be short (unix sun_path; macOS allows 103 bytes, S-0).
	Socket string
	// Home is the agent-owned directory holding the core's XDG state and config.
	Home string
	// TerminalSize is the size programs start at while nobody is attached, COLSxROWS (config `headless`, hc 0.1.16;
	// herdr-core's default is 200x120). Empty: herdr-core's default. An interactive attach resizes a program only while
	// it is attached.
	TerminalSize string
}

// MaxSocketPath is the longest socket path herdr-core accepts on macOS (S-0: "this system allows 103").
const MaxSocketPath = 103

func (o CoreOptions) validate() error {
	switch {
	case o.Session == "" || o.Session == "default":
		return errors.New("hc: a dedicated session name is required (never \"default\")")
	case !filepath.IsAbs(o.Socket):
		return errors.New("hc: socket must be an absolute path")
	case len(o.Socket) > MaxSocketPath:
		return fmt.Errorf("hc: socket path is %d bytes; at most %d are supported", len(o.Socket), MaxSocketPath)
	case !filepath.IsAbs(o.Home):
		return errors.New("hc: home must be an absolute path")
	}
	return nil
}

// env is the isolated environment for hc commands: inherited HC_* variables (for example when the agent itself runs
// inside a herdr-core terminal) are removed so nothing reaches the operator's core.
func (o CoreOptions) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HC_") || k == "XDG_STATE_HOME" || k == "XDG_CONFIG_HOME" || k == "XDG_RUNTIME_DIR" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "HC_SESSION="+o.Session, "HC_SOCKET="+o.Socket,
		"XDG_STATE_HOME="+filepath.Join(o.Home, "state"), "XDG_CONFIG_HOME="+filepath.Join(o.Home, "config"),
		"XDG_RUNTIME_DIR="+filepath.Join(o.Home, "runtime"))
}

// writeConfig writes the core's config.toml: the short socket, notes off (hc types nothing on its own into parent
// agents' terminals) and the size programs start at.
func (o CoreOptions) writeConfig() error {
	for _, d := range []string{"state", "config/herdr-core", "runtime"} {
		if err := os.MkdirAll(filepath.Join(o.Home, d), 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(o.Socket), 0o700); err != nil {
		return err
	}
	cfg := fmt.Sprintf("socket = %q\nnotes = false\n", o.Socket)
	if o.TerminalSize != "" {
		cfg += fmt.Sprintf("headless = %q\n", o.TerminalSize)
	}
	return os.WriteFile(filepath.Join(o.Home, "config", "herdr-core", "config.toml"), []byte(cfg), 0o600)
}

// EnsureCore starts the dedicated core when it is not running and connects to it. Programs of an existing core keep
// running (herdr-core adopts them across core restarts). The config is written either way: a running core reads it for
// the programs it starts next (the terminal size, S-6), so an agent restart applies a changed setting.
func EnsureCore(ctx context.Context, o CoreOptions) (*Client, error) {
	if o.Binary == "" {
		o.Binary = "hc"
	}
	if err := o.validate(); err != nil {
		return nil, err
	}
	if err := o.writeConfig(); err != nil {
		return nil, fmt.Errorf("hc config: %w", err)
	}
	if c, err := Dial(ctx, o.Socket); err == nil {
		return c, nil
	}
	cmd := exec.CommandContext(ctx, o.Binary, "server", "start", "--json") //nolint:gosec // operator-configured binary
	cmd.Env = o.env()
	cmd.Dir = o.Home
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("hc server start: %w: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := Dial(ctx, o.Socket)
		if err == nil {
			return c, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// StopCore stops the dedicated core and its programs. Only tests and explicit operator commands use it.
func StopCore(ctx context.Context, o CoreOptions) error {
	if err := o.validate(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, o.Binary, "server", "stop") //nolint:gosec // operator-configured binary
	cmd.Env = o.env()
	cmd.Dir = o.Home
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hc server stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
