package agent

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR-0068: Claude Code's project key (Claude Code 2.1.317's scheme; the live test checks it against the CLI).
func TestClaudeProjectKey(t *testing.T) {
	for in, want := range map[string]string{
		"/data/spaces/demo-x1y2/go-app": "-data-spaces-demo-x1y2-go-app",
		"/tmp/x.y_z/ä😀":                 "-tmp-x-y-z----",
	} {
		if got := ClaudeProjectKey(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	// Longer than 200 characters: cut and suffixed with the base-36 Java-style string hash of the path.
	long := "/" + strings.Repeat("a", 250)
	got := ClaudeProjectKey(long)
	var h int32
	for _, c := range long {
		h = h*31 + c
	}
	if h < 0 {
		h = -h
	}
	if !strings.HasPrefix(got, "-"+strings.Repeat("a", 199)+"-") || len(got) <= 201 || got[:200] != ClaudeProjectKey(long)[:200] {
		t.Fatalf("long key %s", got)
	}
	if want := got[:200] + "-" + strings.ToLower(itoa36(int64(h))); got != want {
		t.Fatalf("long key %s, want %s", got, want)
	}
}

func itoa36(n int64) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 36 {
		b = append([]byte{digits[n%36]}, b...)
	}
	return string(b)
}

// ClaudeProjectRoot: the repository for a plain checkout and its subfolders, the main checkout for a linked worktree,
// the folder itself outside a repository.
func TestClaudeProjectRoot(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	wt := filepath.Join(root, "worktrees", "w1")
	gitdir := filepath.Join(repo, ".git", "worktrees", "w1")
	plain := filepath.Join(root, "work")
	for _, d := range []string{filepath.Join(repo, "sub"), gitdir, filepath.Join(wt, "pkg"), plain} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitdir, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]string{repo: repo, filepath.Join(repo, "sub"): repo, wt: repo, filepath.Join(wt, "pkg"): repo, plain: plain} {
		if got := ClaudeProjectRoot(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	// A .git file that does not lead to a repository's .git: the folder holding it.
	odd := filepath.Join(root, "odd")
	_ = os.MkdirAll(odd, 0o700)
	_ = os.WriteFile(filepath.Join(odd, ".git"), []byte("not a gitdir line"), 0o600)
	if got := ClaudeProjectRoot(odd); got != odd {
		t.Errorf("odd: %s", got)
	}
	rel := filepath.Join(root, "rel")
	_ = os.MkdirAll(rel, 0o700)
	_ = os.WriteFile(filepath.Join(rel, ".git"), []byte("gitdir: ../repo/.git/worktrees/w1"), 0o600)
	if got := ClaudeProjectRoot(rel); got != repo {
		t.Errorf("relative gitdir: %s", got)
	}
}

// ADR-0068: the auto memory a Claude Code config dir kept is copied out once, under the same project keys, without
// following symbolic links or overwriting what the new folder has; the old copy stays.
func TestPrepareAutoMemory(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "space", "instances", "ins_1", "claude")
	root := filepath.Join(dir, "automemory", "spc_1", "ins_1")
	work := filepath.Join(dir, "space", "work")
	secret := filepath.Join(dir, "space", "instances", "ins_2", "claude", ".credentials.json")
	key := ClaudeProjectKey(work)
	files := map[string]string{
		filepath.Join(claudeDir, "projects", key, "memory", "MEMORY.md"):      "- [a](a.md)\n",
		filepath.Join(claudeDir, "projects", key, "memory", "topics", "a.md"): "a",
		filepath.Join(claudeDir, "projects", "-other", "memory", "MEMORY.md"): "other",
		filepath.Join(claudeDir, "projects", "-other", "s.jsonl"):             "transcript",
		filepath.Join(claudeDir, "projects", "-nomem", "s.jsonl"):             "transcript",
		filepath.Join(claudeDir, "settings.json"):                             "{}",
		secret: "token",
		filepath.Join(root, "projects", "-other", "memory", "MEMORY.md"): "newer",
		filepath.Join(claudeDir, "projects", "-filemem", "memory"):       "a file, not a folder",
	}
	for p, s := range files {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(secret, filepath.Join(claudeDir, "projects", key, "memory", "leak.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(claudeDir, "projects", key), filepath.Join(claudeDir, "projects", "-linked")); err != nil {
		t.Fatal(err)
	}
	got, err := PrepareAutoMemory(claudeDir, root, work)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "projects", key, "memory"); got != want {
		t.Fatalf("dir %s, want %s", got, want)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	if read(filepath.Join(got, "MEMORY.md")) != "- [a](a.md)\n" || read(filepath.Join(got, "topics", "a.md")) != "a" {
		t.Fatal("memory not copied")
	}
	if read(filepath.Join(root, "projects", "-other", "memory", "MEMORY.md")) != "newer" {
		t.Fatal("an existing file was overwritten")
	}
	for _, p := range []string{filepath.Join(got, "leak.md"), filepath.Join(root, "projects", "-linked"), filepath.Join(root, "projects", "-nomem"),
		filepath.Join(root, "projects", "-other", "s.jsonl"), filepath.Join(root, "projects", "-filemem"), filepath.Join(root, "settings.json")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s copied", p)
		}
	}
	if read(filepath.Join(claudeDir, "projects", key, "memory", "MEMORY.md")) == "" {
		t.Fatal("the old copy was removed")
	}
	if fi, err := os.Stat(filepath.Join(got, "MEMORY.md")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}

	// Only once: memory the old folder gets later (an older node's run) is not copied again over the new one.
	_ = os.WriteFile(filepath.Join(claudeDir, "projects", key, "memory", "late.md"), []byte("late"), 0o600)
	if _, err := PrepareAutoMemory(claudeDir, root, work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(got, "late.md")); err == nil {
		t.Fatal("copied twice")
	}

	// A config dir without projects: just the folders.
	fresh := filepath.Join(dir, "automemory", "spc_1", "ins_3")
	got, err = PrepareAutoMemory(filepath.Join(dir, "none"), fresh, work)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("fresh dir: %v", err)
	}

	// A copy that fails is not marked done, so the next launch tries again (and the launch fails rather than losing it).
	broken := filepath.Join(dir, "automemory", "spc_1", "ins_4")
	if err := os.MkdirAll(filepath.Join(broken, "projects", key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "projects", key, "memory"), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareAutoMemory(claudeDir, broken, work); err == nil {
		t.Fatal("a failed copy succeeded")
	}
	if _, err := os.Stat(filepath.Join(broken, ".migrated")); err == nil {
		t.Fatal("a failed copy was marked done")
	}
}

// A copy that fails midway leaves no partial file (nor its temporary file) for the retry to skip; the retry copies it
// whole.
func TestPrepareAutoMemoryPartialCopy(t *testing.T) {
	dir := t.TempDir()
	claudeDir := filepath.Join(dir, "claude")
	root := filepath.Join(dir, "automemory")
	work := filepath.Join(dir, "work")
	key := ClaudeProjectKey(work)
	src := filepath.Join(claudeDir, "projects", key, "memory", "MEMORY.md")
	if err := os.MkdirAll(filepath.Dir(src), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("the whole memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := copyData
	t.Cleanup(func() { copyData = orig })
	copyData = func(dst io.Writer, _ io.Reader) (int64, error) {
		n, _ := dst.Write([]byte("the wh"))
		return int64(n), errors.New("disk full")
	}
	if _, err := PrepareAutoMemory(claudeDir, root, work); err == nil {
		t.Fatal("a failed copy succeeded")
	}
	mem := filepath.Join(root, "projects", key, "memory")
	entries, _ := os.ReadDir(mem)
	if len(entries) != 0 {
		t.Fatalf("left behind: %v", entries)
	}
	copyData = orig
	got, err := PrepareAutoMemory(claudeDir, root, work)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(got, "MEMORY.md")); string(b) != "the whole memory" {
		t.Fatalf("retry copied %q", b)
	}
	if fi, err := os.Stat(filepath.Join(got, "MEMORY.md")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
}
