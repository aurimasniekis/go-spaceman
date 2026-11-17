package agent

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Harness configuration never travels through role memory (ADR-0025 addendum). The memory folder is handed to Claude
// Code with --add-dir, which loads .claude/skills and .claude/commands from it; Codex and MCP clients read similar
// folders. Role memory is shared by every instance of a role, so a skill or command one instance wrote would run in
// all of them. Notes such as CLAUDE.md and AGENTS.md stay: they are text the model reads, not code.
//
// Two daemon-written files in each checkout's .git enforce it, so neither side depends on what an agent does:
//   - info/exclude: such paths are never staged, at any depth;
//   - info/sparse-checkout (core.sparseCheckout): such paths are never written into the working tree when history
//     that already holds them (pushed by an older agent) is checked out, reset to or fast-forwarded onto.

// harnessDirs are directory names that carry harness configuration, matched at any depth.
var harnessDirs = []string{".claude", ".codex", ".agents"}

// harnessFiles are top-level file names that carry harness configuration.
var harnessFiles = []string{".mcp.json"}

// isHarnessPath reports whether a repository-relative path is, or lies under, harness configuration.
func isHarnessPath(p string) bool {
	p = path.Clean(filepath.ToSlash(p))
	parts := strings.Split(p, "/")
	for _, part := range parts[:len(parts)-1] {
		if isHarnessName(part) {
			return true
		}
	}
	last := parts[len(parts)-1]
	if isHarnessName(last) {
		return true
	}
	return len(parts) == 1 && last != "" && containsStr(harnessFiles, last)
}

func isHarnessName(n string) bool { return containsStr(harnessDirs, n) }

func containsStr(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func harnessExclude() string {
	var b strings.Builder
	b.WriteString("# written by SpaceMan: harness configuration never travels through role memory\n")
	for _, d := range harnessDirs {
		b.WriteString(d + "/\n")
	}
	for _, f := range harnessFiles {
		b.WriteString("/" + f + "\n")
	}
	return b.String()
}

func harnessSparse() string {
	var b strings.Builder
	b.WriteString("/*\n")
	for _, d := range harnessDirs {
		b.WriteString("!" + d + "/\n")
	}
	for _, f := range harnessFiles {
		b.WriteString("!/" + f + "\n")
	}
	return b.String()
}

// guard (re)writes the checkout's exclude and sparse-checkout files and applies them to the working tree. It is run
// before a checkout is created from, moved onto, or staged for shared history.
func (m *memManager) guard(ctx context.Context, inst string) error {
	info := filepath.Join(inst, ".git", "info")
	if err := os.MkdirAll(info, 0o700); err != nil {
		return err
	}
	// The exclude file keeps whatever else it holds; the sparse-checkout file is SpaceMan's alone.
	ex := filepath.Join(info, "exclude")
	cur, _ := os.ReadFile(ex) //nolint:gosec // the checkout's own .git
	if !strings.Contains(string(cur), harnessExclude()) {
		body := string(cur)
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := os.WriteFile(ex, []byte(body+harnessExclude()), 0o600); err != nil { //nolint:gosec // the checkout's own .git
			return err
		}
	}
	if b, err := os.ReadFile(filepath.Join(info, "sparse-checkout")); err != nil || string(b) != harnessSparse() { //nolint:gosec // the checkout's own .git
		if err := os.WriteFile(filepath.Join(info, "sparse-checkout"), []byte(harnessSparse()), 0o600); err != nil {
			return err
		}
	}
	if out, _ := m.git.Run(ctx, inst, nil, "config", "--local", "--get", "core.sparseCheckout"); strings.TrimSpace(out) != "true" {
		if _, err := m.git.Run(ctx, inst, nil, "config", "--local", "core.sparseCheckout", "true"); err != nil {
			return err
		}
		if _, err := m.git.Run(ctx, inst, nil, "config", "--local", "core.sparseCheckoutCone", "false"); err != nil {
			return err
		}
		if m.git.RevParse(ctx, inst, "HEAD") != "" {
			// An existing checkout that already holds such paths loses them from the working tree now.
			if _, err := m.git.Run(ctx, inst, nil, "sparse-checkout", "reapply"); err != nil {
				return err
			}
		}
	}
	return nil
}
