package agent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
		b.WriteString(d + "\n")
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
		b.WriteString("!" + d + "\n")
	}
	for _, f := range harnessFiles {
		b.WriteString("!/" + f + "\n")
	}
	return b.String()
}

// guard (re)writes the checkout's exclude and sparse-checkout files and applies them to the working tree. It is run
// before a checkout is created from, moved onto, or staged for shared history. The files are written through an
// os.Root on the checkout's .git, and every component is looked at first: a symlinked .git, info or file, or a
// hard-linked file, would otherwise let the write reach a file outside the checkout.
func (m *memManager) guard(ctx context.Context, inst string) error {
	// .git is opened through the checkout's own root, which refuses a link that leaves it, and is looked at without
	// following links, so a symlinked .git is refused too.
	top, err := os.OpenRoot(inst)
	if err != nil {
		return err
	}
	defer func() { _ = top.Close() }()
	if fi, err := top.Lstat(".git"); err != nil || !fi.IsDir() {
		return errors.New("memory checkout: .git is not a directory")
	}
	root, err := top.OpenRoot(".git")
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if fi, err := root.Lstat("info"); errors.Is(err, fs.ErrNotExist) {
		if err := root.Mkdir("info", 0o700); err != nil {
			return err
		}
	} else if err != nil || !fi.IsDir() {
		return errors.New("memory checkout: .git/info is not a directory")
	}
	// The exclude file keeps whatever else it holds; the sparse-checkout file is SpaceMan's alone.
	cur, err := guardRead(root, "info/exclude")
	if err != nil {
		return err
	}
	if !strings.Contains(cur, harnessExclude()) {
		body := cur
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := guardWrite(root, "info/exclude", body+harnessExclude()); err != nil {
			return err
		}
	}
	if cur, err := guardRead(root, "info/sparse-checkout"); err != nil {
		return err
	} else if cur != harnessSparse() {
		if err := guardWrite(root, "info/sparse-checkout", harnessSparse()); err != nil {
			return err
		}
	}
	// git config writes through a lock file that follows a symlinked .git/config (and creates a dangling target).
	if _, err := guardFile(root, "config"); err != nil {
		return err
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

// guardFile looks at a file inside the root: it must be absent or a regular file nobody else links to.
func guardFile(root *os.Root, name string) (exists bool, err error) {
	fi, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() || linkCount(fi) > 1 {
		return true, fmt.Errorf("memory checkout: .git/%s is not a plain file", name)
	}
	return true, nil
}

func guardRead(root *os.Root, name string) (string, error) {
	exists, err := guardFile(root, name)
	if err != nil || !exists {
		return "", err
	}
	b, err := root.ReadFile(name)
	return string(b), err
}

func guardWrite(root *os.Root, name, body string) error {
	if _, err := guardFile(root, name); err != nil {
		return err
	}
	// Write a new file beside the old one and rename it over: nothing is truncated in place, and a link planted at
	// the name after the check is replaced, not followed.
	tmp := name + ".spaceman-tmp"
	_ = root.Remove(tmp)
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(body); err != nil {
		_ = f.Close()
		_ = root.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	if err := root.Rename(tmp, name); err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return nil
}
