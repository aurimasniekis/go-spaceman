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

// TestRealCodexRun (SM-ACCT-002/004, SM-HARN-001, SM-RUN-002, SM-VER-002, ADR-0053): a real, interactive Codex run
// started through SpaceMan with a credential held only as a SpaceMan account. Opt-in and billed: it needs
// SPACEMAN_REAL_CODEX=1 and either OPENAI_API_KEY (an openai_api_key account) or SPACEMAN_REAL_CODEX_AUTH_FILE, the
// auth.json of a ChatGPT login (a codex_chatgpt_login account). Use a dedicated login for the latter: Codex may refresh
// it during the run, and a refresh token works once, so the original file could stop working (sign in again in a
// spare CODEX_HOME to get a copy for the test). SPACEMAN_REAL_CODEX_MODEL picks the model (default: Codex's own). One
// short prompt, read-only sandbox, no approvals; the run is stopped afterwards.
//
// It checks that Codex answers (the answer is not in the prompt), that herdr-core reports its turn as done through
// OSC 7501 (verified for Claude Code, not yet for Codex; terminal notices depend on it), and that the credential is
// at rest only where ADR-0009 and ADR-0053 put it.
func TestRealCodexRun(t *testing.T) {
	if os.Getenv("SPACEMAN_REAL_CODEX") != "1" {
		t.Skip("set SPACEMAN_REAL_CODEX=1 and OPENAI_API_KEY or SPACEMAN_REAL_CODEX_AUTH_FILE to run a real, billed Codex run")
	}
	kind, secret := "openai_api_key", os.Getenv("OPENAI_API_KEY")
	if f := os.Getenv("SPACEMAN_REAL_CODEX_AUTH_FILE"); f != "" {
		b, err := os.ReadFile(f)
		must(t, err)
		kind, secret = "codex_chatgpt_login", string(b)
	}
	if secret == "" {
		t.Skip("set OPENAI_API_KEY or SPACEMAN_REAL_CODEX_AUTH_FILE")
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	e := newEnvHarnesses(t, "", codex)
	_, spaceID := e.setupSpaceWith(t, kind, secret)
	launch := map[string]any{"kind": "worker", "role": "reviewer", "harness": "codex",
		"settings": map[string]string{"sandbox": "read-only", "approval": "never"},
		"prompt":   "Reply with the word READY followed by the sum of 20 and 22, and nothing else. Do not run any commands."}
	if m := os.Getenv("SPACEMAN_REAL_CODEX_MODEL"); m != "" {
		launch["model"] = m
	}
	runID := e.launch(t, spaceID, launch)

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
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) && !answered.MatchString(output()) {
		time.Sleep(time.Second)
	}
	if !answered.MatchString(output()) {
		t.Fatalf("Codex did not answer within 3 minutes; the screen ends with:\n%s", tail(output(), 3000))
	}
	// The turn ending, as herdr-core sees it (Codex's OSC 7501 emitter, SM-RUN-002).
	statusDeadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(statusDeadline) {
		e.api("GET", "/spaces/"+spaceID+"/runs/"+runID, nil, &status)
		if status.Status == "done" || status.Status == "idle" {
			break
		}
		time.Sleep(time.Second)
	}
	if status.Status != "done" && status.Status != "idle" {
		t.Errorf("Codex answered but herdr-core reports status %q (turn %d): without OSC 7501 status, terminal notices never reach Codex runs", status.Status, status.Turn)
	}

	// Where the credential is at rest.
	if _, err := os.Stat(filepath.Join(runDir, "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("secret file not unlinked by the launcher shim: %v", err)
	}
	probe := secret
	if kind == "codex_chatgpt_login" {
		// The login lives in the node's login folder (ADR-0053); its refresh token must not appear elsewhere.
		logins, _ := filepath.Glob(filepath.Join(e.dir, "agent", "logins", "*", "auth.json"))
		if len(logins) != 1 {
			t.Fatalf("node login copies: %v", logins)
		}
		if m := regexp.MustCompile(`"refresh_token"\s*:\s*"([^"]{12,})"`).FindStringSubmatch(secret); m != nil {
			probe = m[1]
		}
	}
	for _, f := range []string{
		filepath.Join(e.hcHome, "state", "herdr-core", e.session, "terminals.json"),
		filepath.Join(e.dir, "central.log"), filepath.Join(e.dir, "agent.log"),
	} {
		if b, _ := os.ReadFile(f); strings.Contains(string(b), probe) {
			t.Fatalf("the credential is stored in clear in %s", filepath.Base(f))
		}
	}
	_ = filepath.Walk(filepath.Join(e.dir, "central"), func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), probe) {
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
	t.Logf("real Codex run %s (%s account) answered with status %q and was stopped", runID, kind, status.Status)
}
