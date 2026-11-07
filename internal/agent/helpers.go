package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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
	env := scrubEnv(os.Environ())
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

// inherited reports environment variables a run must never inherit from the node agent or the herdr-core server:
// another Claude Code or Codex session's markers (a nested-session marker turns off transcripts, verified with Claude
// Code 2.1.316) and provider credentials from the node's environment, which would bypass SpaceMan accounts
// (SM-ACCT-004). The run's own credentials come from the secrets file, applied after scrubbing.
func inherited(name string) bool {
	switch name {
	case "CLAUDECODE", "CLAUDE_PID", "CLAUDE_EFFORT", "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "CODEX_THREAD_ID":
		return true
	}
	return strings.HasPrefix(name, "CLAUDE_CODE_") || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CODEX_SANDBOX")
}

func scrubEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !inherited(name) {
			out = append(out, kv)
		}
	}
	return out
}

// MergeJSONFile applies patch to the JSON object in path (see harness.Launch.MergeJSON), creating it when missing. The
// file is replaced atomically and stays 0600.
func MergeJSONFile(path string, patch map[string]any) error {
	return editJSONFile(path, func(doc map[string]any) { mergeInto(doc, patch) })
}

// OwnJSONKeysFile sets SpaceMan's top-level keys in the JSON object in path (see harness.Launch.OwnJSON): each key is
// set, keys it set at an earlier call and no longer gives are removed, and every other key is kept. The keys it owns
// are recorded next to the file (<path>.spaceman-keys). Both files are created when missing and replaced atomically.
//
// A record that is not a JSON list of strings can't say which keys an earlier profile set (deny rules or hooks it no
// longer has), and guessing could keep them. The settings file and the record are then moved aside
// (<name>.corrupt-<unix time>), a warning names both, and the call starts from an empty settings file: no stale key
// survives, and the instance recovers without anyone fixing the node by hand. The record is written before the
// settings, with every key that may be in them (the earlier and the new ones), and narrowed to the new keys after: an
// interrupted call never leaves a key in the settings that the record does not name, so the next call can still
// remove it. log may be nil.
func OwnJSONKeysFile(log *slog.Logger, path string, keys map[string]any) error {
	record := path + ".spaceman-keys"
	var before []string
	if b, err := os.ReadFile(record); err == nil { //nolint:gosec // agent-owned path
		if err := json.Unmarshal(b, &before); err != nil {
			if err := setAsideCorrupt(log, path, record); err != nil {
				return err
			}
			before = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	owned := make([]string, 0, len(keys))
	for k := range keys {
		owned = append(owned, k)
	}
	sort.Strings(owned)
	writeRecord := func(names []string) error {
		b, err := json.Marshal(names)
		if err != nil {
			return err
		}
		return writeAtomic(record, b)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	pending := append(slices.Clone(before), owned...)
	sort.Strings(pending)
	if err := writeRecord(slices.Compact(pending)); err != nil {
		return err
	}
	if err := editJSONFile(path, func(doc map[string]any) {
		for _, k := range before {
			if _, still := keys[k]; !still {
				delete(doc, k)
			}
		}
		for k, v := range keys {
			doc[k] = v
		}
	}); err != nil {
		return err
	}
	return writeRecord(owned)
}

// setAsideCorrupt moves the settings file and its corrupt keys record out of the way (renamed with a .corrupt-<unix
// time> suffix) and logs a warning naming both new paths. A missing settings file is not an error.
func setAsideCorrupt(log *slog.Logger, path, record string) error {
	suffix := fmt.Sprintf(".corrupt-%d", time.Now().Unix())
	movedSettings := path + suffix
	if err := os.Rename(path, movedSettings); errors.Is(err, os.ErrNotExist) {
		movedSettings = ""
	} else if err != nil {
		return fmt.Errorf("%s: corrupt keys record; moving the settings aside: %w", record, err)
	}
	movedRecord := record + suffix
	if err := os.Rename(record, movedRecord); err != nil {
		return fmt.Errorf("%s: corrupt keys record; moving it aside: %w", record, err)
	}
	if log != nil {
		log.Warn("the record of SpaceMan's settings keys is not a JSON list of key names; settings moved aside and started fresh",
			slog.String("settings_moved_to", movedSettings), slog.String("record_moved_to", movedRecord))
	}
	return nil
}

func editJSONFile(path string, edit func(map[string]any)) error {
	doc := map[string]any{}
	b, err := os.ReadFile(path) //nolint:gosec // agent-owned path
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &doc); err != nil || doc == nil {
			return fmt.Errorf("%s: not a JSON object", path)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	edit(doc)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".spaceman-tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mergeInto(dst, patch map[string]any) {
	for k, v := range patch {
		if pv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				mergeInto(dv, pv)
				continue
			}
			fresh := map[string]any{}
			mergeInto(fresh, pv)
			dst[k] = fresh
			continue
		}
		dst[k] = v
	}
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
