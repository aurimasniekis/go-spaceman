//go:build real

package real

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/agent"
	"github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// TestRealClaudeCLIReadsSavedAttachment (ADR-0073, T-42): a screenshot larger than an inline tool result, saved where
// spaceman_attachment_save puts it (<space>/instances/<instance>/attachments/<run>/<attachment>/<name>, 0600), opens
// with the real Claude Code CLI's own Read tool as an image, with no permission dialog, under exactly the settings
// SpaceMan writes for a run (no directories from the profile). The PermissionRequest hook's binary is missing, so a
// dialog would show up as SpaceMan's deny. No model and no credentials: a local fake API scripts the tool call (print
// mode, a throwaway HOME, config dir and space folder). Opt-in: SPACEMAN_REAL_CLAUDE_CLI=1 with claude on PATH.
func TestRealClaudeCLIReadsSavedAttachment(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_CLAUDE_CLI") != "1" {
		t.Skip("set SPACEMAN_REAL_CLAUDE_CLI=1 (with claude on PATH) to check reading a saved attachment with the real Claude Code CLI")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	space := filepath.Join(root, "spaces", "space")
	home := filepath.Join(root, "home")
	work := filepath.Join(space, "work")
	cfg := filepath.Join(space, "instances", "ins_a")
	claudeDir := filepath.Join(cfg, "claude")
	saved := filepath.Join(cfg, "attachments", "run_1", "att_1", "Screenshot_2026-11-08.png")
	for _, d := range []string{home, work, claudeDir, filepath.Dir(saved)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A real PNG over 512 KiB: noise does not compress.
	img := image.NewNRGBA(image.Rect(0, 0, 1000, 700))
	_, _ = rand.Read(img.Pix)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if buf.Len() <= 512<<10 {
		t.Fatalf("test image is only %d bytes", buf.Len())
	}
	if err := os.WriteFile(saved, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	autoMem, err := agent.PrepareAutoMemory(claudeDir, filepath.Join(root, "automemory", "spc_1", "ins_a"), work)
	if err != nil {
		t.Fatal(err)
	}
	l, err := harness.Claude{}.Build(harness.Context{AgentBinary: filepath.Join(root, "missing", "spaceman-agent"), ConfigDir: cfg, SpaceDir: space,
		SpaceInstances: []string{"ins_a"}, WorkDir: work, RunID: "run_1", SpaceID: "spc_1", RunDir: root, Binary: claude,
		Payload: proto.LaunchPayload{Kind: "worker"}, AutoMemoryDir: autoMem})
	if err != nil {
		t.Fatal(err)
	}
	for p, keys := range l.OwnJSON {
		if err := agent.OwnJSONKeysFile(nil, p, keys, l.StripJSON[p]...); err != nil {
			t.Fatal(err)
		}
	}
	for p, patch := range l.MergeJSON {
		if err := agent.MergeJSONFile(p, patch); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(root, "outside.png")
	if err := os.WriteFile(outside, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	expect := []struct {
		what   string
		path   string
		denied bool
		has    string
	}{
		{"read the saved attachment", saved, false, `"type":"image"`},
		{"read the same image outside the space folder (control: a dialog, denied by the hook)", outside, true, "SpaceMan denied this request"},
	}
	api := &scriptedAPI{results: map[string]toolResult{}}
	for _, e := range expect {
		api.script = append(api.script, map[string]any{"name": "Read", "input": map[string]any{"file_path": e.path}})
	}
	srv := httptest.NewServer(api)
	defer srv.Close()
	for _, mode := range []string{"acceptEdits", "default"} {
		api.mu.Lock()
		api.next, api.results = 0, map[string]toolResult{}
		api.mu.Unlock()
		cmd := exec.Command(claude, "-p", "go", "--model", "claude-sonnet-4-5", "--permission-mode", mode) //nolint:gosec // the test's own CLI
		cmd.Dir = work
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "CLAUDE_CONFIG_DIR=" + claudeDir,
			"ANTHROPIC_BASE_URL=" + srv.URL, "ANTHROPIC_AUTH_TOKEN=dummy", "DISABLE_AUTOUPDATER=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("claude (%s): %v\n%s", mode, err, out)
		}
		api.mu.Lock()
		for i, e := range expect {
			r, ok := api.results[fmt.Sprintf("toolu_%02d", i)]
			if !ok || r.IsError != e.denied || !strings.Contains(r.Text, e.has) {
				t.Errorf("%s (%s): error=%v answered=%v %.300s", e.what, mode, r.IsError, ok, r.Text)
			}
		}
		api.mu.Unlock()
	}
}
