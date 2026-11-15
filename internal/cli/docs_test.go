package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestGenerateDocs(t *testing.T) {
	t.Parallel()
	root := newRootFor(&App{SpecSource: specOnce})
	root.AddCommand(&cobra.Command{Use: "hidden", Hidden: true, Run: func(*cobra.Command, []string) {}})
	files := GenerateDocs(root)
	again := GenerateDocs(newRootFor(&App{SpecSource: specOnce}))
	for name, b := range again {
		if !bytes.Equal(files[name], b) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
	if _, ok := files["hidden.md"]; ok {
		t.Fatal("hidden command documented")
	}
	idx := string(files["index.md"])
	for _, s := range []string{"## Exit codes", "`--token-file`", "[`spaceman space`](./space)", "| `3` | conflict"} {
		if !strings.Contains(idx, s) {
			t.Errorf("index lacks %q", s)
		}
	}
	space := string(files["space.md"])
	for _, s := range []string{"## spaceman space relocate start", "API: `start-relocation`", "`--acknowledge-loss`", "spaceman space relocate start <space> [flags]",
		"[`spaceman space relocate status`](#spaceman-space-relocate-status)", "Aliases: `spaces`."} {
		if !strings.Contains(space, s) {
			t.Errorf("space.md lacks %q", s)
		}
	}
	// No raw angle brackets or Vue interpolation outside code (VitePress would parse them).
	for name, b := range files {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "spaceman ") || strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "<!--") {
				continue
			}
			if strings.Contains(stripCode(line), "<") || strings.Contains(stripCode(line), "{{") {
				t.Errorf("%s: unescaped markup: %s", name, line)
			}
		}
	}
	if got := escapeMD("a|b <c> {d} `<e>`\nf"); got != `a\|b &lt;c&gt; &#123;d&#125; `+"`<e>`"+` f` {
		t.Fatalf("escape %q", got)
	}
	ops := Operations(root)
	if len(ops) < 120 || !contains(ops, "get-xspace-peer") {
		t.Fatalf("operations %d", len(ops))
	}

	dir := t.TempDir()
	stale := filepath.Join(dir, "gone.md")
	if err := os.WriteFile(stale, []byte(docsHeader+"# old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "handwritten.md")
	if err := os.WriteFile(mine, []byte("# mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteDocs(root, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale generated page kept")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatal("hand-written page removed")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "space.md")); !bytes.Equal(b, files["space.md"]) {
		t.Fatal("written page differs")
	}
	if err := WriteDocs(root, filepath.Join(mine, "sub")); err == nil {
		t.Fatal("unwritable directory accepted")
	}
}

// stripCode removes `code spans` from a Markdown line.
func stripCode(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		if r == '`' {
			in = !in
			continue
		}
		if !in {
			b.WriteRune(r)
		}
	}
	return b.String()
}
