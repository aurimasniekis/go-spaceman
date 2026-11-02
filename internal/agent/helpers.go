package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// LaunchShim is `spaceman-agent launch --secrets-file F -- argv…`: it reads the run's secret environment from F (0600,
// written by the agent), unlinks it, adds it to the environment and replaces itself with the harness (exec). The
// secrets therefore exist only in the harness process — never in herdr-core's persisted terminal state (S-1,
// ADR-0009). A missing file means the run has no secrets.
func LaunchShim(secretsFile string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("launch: no command")
	}
	env := os.Environ()
	if secretsFile != "" {
		b, err := os.ReadFile(secretsFile) //nolint:gosec // agent-owned path
		switch {
		case err == nil:
			_ = os.Remove(secretsFile)
			var m map[string]string
			if err := json.Unmarshal(b, &m); err != nil {
				return fmt.Errorf("launch: secrets file: %w", err)
			}
			for k, v := range m {
				if strings.ContainsAny(k, "=\x00") {
					continue
				}
				env = append(env, k+"="+v)
			}
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("launch: secrets file: %w", err)
		}
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	return syscall.Exec(path, argv, env) //nolint:gosec // the harness argv built by the agent
}

// LeaseGuard is `spaceman-agent lease-guard --lease-file F`: it runs as the parent terminal of a space's runs and
// exits once the lease file's valid_until has passed or the file is missing or invalid; herdr-core then ends every
// run terminal parented to it (S-2, ADR-0017). It depends only on the local clock and the file, so it holds the bound
// even when the daemon is dead.
func LeaseGuard(ctx context.Context, leaseFile string, now func() time.Time) error {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		var lf LeaseFile
		b, err := os.ReadFile(leaseFile) //nolint:gosec // agent-owned path
		if err != nil || json.Unmarshal(b, &lf) != nil || lf.ValidUntil.IsZero() {
			return fmt.Errorf("lease guard: lease file unreadable; ending the space's runs")
		}
		if !now().Before(lf.ValidUntil) {
			return fmt.Errorf("lease guard: lease expired at %s; ending the space's runs", lf.ValidUntil.Format(time.RFC3339))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
