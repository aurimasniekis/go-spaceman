package agent

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// autoMemoryDir is the folder that holds one instance's Claude Code auto memory: <data_dir>/automemory/<space>/<instance>,
// outside the space folder, so the other runs of the space, whose file tools may write the space folder, can't reach it
// (ADR-0068). Inside it, the layout is Claude Code's own: projects/<project key>/memory.
func (a *Agent) autoMemoryDir(spaceID, instanceID string) string {
	return filepath.Join(a.o.DataDir, "automemory", spaceID, instanceID)
}

// PrepareAutoMemory returns the auto memory folder for a run of the instance whose Claude Code config dir is claudeDir
// and whose working directory is work: root/projects/<key>/memory, with Claude Code's key for work's project. The first
// time the instance's root is used, the auto memory Claude Code kept in the config dir before (claudeDir/projects/*/memory,
// now denied to the run's file tools) is copied into root under the same keys, so it keeps working; the old copy stays
// where it is. Only folders and regular files are copied, never symbolic links (a run could have planted one pointing
// at another instance's login). The root is only marked done after a full copy, so a failed copy is retried.
func PrepareAutoMemory(claudeDir, root, work string) (string, error) {
	dir := filepath.Join(root, "projects", ClaudeProjectKey(ClaudeProjectRoot(work)), "memory")
	done := filepath.Join(root, ".migrated")
	if _, err := os.Stat(done); errors.Is(err, fs.ErrNotExist) {
		if err := migrateAutoMemory(claudeDir, root); err != nil {
			return "", err
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(done, nil, 0o600); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// migrateAutoMemory copies claudeDir/projects/<key>/memory into root/projects/<key>/memory for every project that has
// one, keeping files already in root.
func migrateAutoMemory(claudeDir, root string) error {
	projects, err := os.ReadDir(filepath.Join(claudeDir, "projects"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	for _, p := range projects {
		if !p.IsDir() || p.Type()&fs.ModeSymlink != 0 {
			continue
		}
		src := filepath.Join(claudeDir, "projects", p.Name(), "memory")
		if fi, err := os.Lstat(src); err != nil || !fi.IsDir() {
			continue
		}
		if err := copyTree(src, filepath.Join(root, "projects", p.Name(), "memory")); err != nil {
			return err
		}
	}
	return nil
}

// copyTree copies the folders and regular files under src into dst (0700 folders, 0600 files), skipping everything
// else and files dst already has.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case !d.Type().IsRegular():
			return nil
		}
		if _, err := os.Lstat(target); err == nil {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a regular file under the instance's own config dir (WalkDir does not follow links)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is under the instance's auto memory root
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ClaudeProjectRoot is the folder Claude Code keys a working directory's project state by: the main checkout of the
// git repository work is in (for a linked worktree, the repository its .git file points at), or work itself outside a
// repository (checked against Claude Code 2.1.317, docs/contracts/harnesses.md).
func ClaudeProjectRoot(work string) string {
	for d := work; ; {
		g := filepath.Join(d, ".git")
		if fi, err := os.Stat(g); err == nil { //nolint:gosec // the run's own working directory
			if fi.IsDir() {
				return d
			}
			if common := worktreeCommonDir(g); common != "" && filepath.Base(common) == ".git" {
				return filepath.Dir(common)
			}
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return work
		}
		d = parent
	}
}

// worktreeCommonDir follows a linked worktree's .git file (gitdir: <repo>/.git/worktrees/<name>) to the repository's
// common git dir, or returns "".
func worktreeCommonDir(gitFile string) string {
	b, err := os.ReadFile(gitFile) //nolint:gosec // the run's own working directory
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(b), "\n")
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(line), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	c, err := os.ReadFile(filepath.Join(gitdir, "commondir")) //nolint:gosec // git's own metadata of the run's checkout
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(c))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitdir, common)
	}
	return filepath.Clean(common)
}

// ClaudeProjectKey is Claude Code's folder name for a project path: every character other than an ASCII letter or
// digit becomes "-", and a name longer than 200 characters is cut there and gets "-" and a base-36 hash of the path
// (Claude Code 2.1.317's own scheme, so memory it kept under projects/<key> is found under the same key).
func ClaudeProjectKey(path string) string {
	var out strings.Builder
	for _, r := range path {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			out.WriteRune(r)
			continue
		}
		// JavaScript replaces each UTF-16 code unit: a character outside the BMP becomes two dashes.
		for range utf16.Encode([]rune{r}) {
			out.WriteByte('-')
		}
	}
	key := out.String()
	if len(key) <= 200 {
		return key
	}
	var h int32
	for _, u := range utf16.Encode([]rune(path)) {
		h = h<<5 - h + int32(u)
	}
	abs := int64(h)
	if abs < 0 {
		abs = -abs
	}
	return key[:200] + "-" + strconv.FormatInt(abs, 36)
}
