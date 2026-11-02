//go:build real

package real

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/recording"
)

// ansi matches terminal escape sequences (CSI and OSC), so the recorded screen output can be searched as text.
var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// TestRealClaudeRun (SM-ACCT-002/004/005, SM-HARN-001, K-10): a real, interactive Claude Code run started through
// SpaceMan with a subscription token held only as a SpaceMan account. Opt-in and budget-bounded: it needs
// SPACEMAN_REAL_CLAUDE=1 and CLAUDE_CODE_OAUTH_TOKEN (from `claude setup-token`), uses the Haiku model and one short
// prompt (well under $0.05), and stops the run afterwards. Every process the test starts gets an environment without
// Claude or Anthropic variables, so the credential can only arrive through the account.
func TestRealClaudeRun(t *testing.T) {
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if os.Getenv("SPACEMAN_REAL_CLAUDE") != "1" || token == "" {
		t.Skip("set SPACEMAN_REAL_CLAUDE=1 and CLAUDE_CODE_OAUTH_TOKEN to run a real, billed Claude Code run")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	e := newEnvWith(t, claude)
	_, spaceID := e.setupSpace(t, token)
	runID := e.launch(t, spaceID, map[string]any{"kind": "worker", "role": "reviewer", "harness": "claude-code", "model": "haiku",
		"prompt": "Reply with the word READY followed by the sum of 20 and 22, and nothing else. Do not use any tools."})

	// The harness answers (the sum is not in the prompt), and herdr-core reports the turn as done through OSC 7501.
	runDir := filepath.Join(e.dir, "agent", "runs", runID)
	output := func() string {
		res, err := recording.Read(filepath.Join(runDir, "recording"), 0)
		if err != nil {
			return ""
		}
		var out strings.Builder
		for _, rec := range res.Records {
			out.Write(rec.Data)
		}
		return ansi.ReplaceAllString(out.String(), "")
	}
	answered := regexp.MustCompile(`READY\s*42`)
	var status struct {
		Status string `json:"status"`
		Turn   int64  `json:"turn"`
	}
	eventually(t, 120*time.Second, "Claude answered", func() bool {
		e.api("GET", "/spaces/"+spaceID+"/runs/"+runID, nil, &status)
		return answered.MatchString(output()) && status.Turn >= 1 && status.Status == "done"
	})
	if strings.Contains(output(), "Not logged in") || strings.Contains(output(), "Yes, I trust this folder") {
		t.Fatal("the run stopped at login or folder trust")
	}

	// The credential never landed anywhere at rest: the shim consumed its file, herdr-core's state, the central
	// database and both logs do not contain it.
	if _, err := os.Stat(filepath.Join(runDir, "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("secret file not unlinked by the launcher shim: %v", err)
	}
	for _, f := range []string{
		filepath.Join(e.hcHome, "state", "herdr-core", e.session, "terminals.json"),
		filepath.Join(e.dir, "central.log"), filepath.Join(e.dir, "agent.log"),
	} {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), token) {
			t.Fatalf("the credential is stored in clear in %s", filepath.Base(f))
		}
	}
	_ = filepath.Walk(filepath.Join(e.dir, "central"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
				t.Errorf("the credential is stored in clear in central's %s", p)
			}
		}
		return nil
	})

	if st := e.api("POST", "/spaces/"+spaceID+"/runs/"+runID+"/stop", map[string]any{"mode": "kill", "reason": "real test done"}, nil); st != 200 {
		t.Fatalf("stop: %d", st)
	}
	eventually(t, 30*time.Second, "run ended", func() bool {
		o := e.getRun(spaceID, runID).Observed
		return o == "exited" || o == "failed"
	})
	t.Logf("real Claude run %s answered and was stopped", runID)
}

// TestRealClaudeUsesSpaceManTools (SM-MCP-001/003, ADR-0012/0022): a real Claude Code run calls a SpaceMan tool through
// its MCP bridge, the node connection and central; the inbox item it adds is attributed to the run. Opt-in and
// budget-bounded like TestRealClaudeRun.
func TestRealClaudeUsesSpaceManTools(t *testing.T) {
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if os.Getenv("SPACEMAN_REAL_CLAUDE") != "1" || token == "" {
		t.Skip("set SPACEMAN_REAL_CLAUDE=1 and CLAUDE_CODE_OAUTH_TOKEN to run a real, billed Claude Code run")
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	e := newEnvWith(t, claude)
	_, spaceID := e.setupSpace(t, token)
	runID := e.launch(t, spaceID, map[string]any{"kind": "worker", "role": "reviewer", "harness": "claude-code", "model": "haiku",
		"prompt": "Use the spaceman_inbox_add tool exactly once with body \"Real tool check\" and title \"Real tool check\". Then reply with the word FINISHED and nothing else."})
	var items struct {
		Items []struct {
			Title      string `json:"title"`
			SourceKind string `json:"source_kind"`
			SourceID   string `json:"source_id"`
		} `json:"items"`
	}
	eventually(t, 150*time.Second, "Claude added an inbox item through SpaceMan", func() bool {
		e.api("GET", "/spaces/"+spaceID+"/inbox", nil, &items)
		for _, it := range items.Items {
			if it.Title == "Real tool check" && it.SourceKind == "worker" && it.SourceID == runID {
				return true
			}
		}
		return false
	})
	if st := e.api("POST", "/spaces/"+spaceID+"/runs/"+runID+"/stop", map[string]any{"mode": "kill", "reason": "real test done"}, nil); st != 200 {
		t.Fatalf("stop: %d", st)
	}
	t.Logf("real Claude run %s used spaceman_inbox_add", runID)
}
