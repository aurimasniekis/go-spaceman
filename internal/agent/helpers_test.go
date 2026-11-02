package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The launcher shim never passes another harness session's markers or node-level provider keys to a run.
func TestScrubEnv(t *testing.T) {
	in := []string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=1", "CLAUDE_CODE_OAUTH_TOKEN=node", "ANTHROPIC_API_KEY=node",
		"OPENAI_API_KEY=node", "CODEX_ACCESS_TOKEN=node", "CODEX_SANDBOX_NETWORK_DISABLED=1", "CLAUDE_CONFIG_DIR=/x", "CODEX_HOME=/y", "HC_TERMINAL_ID=t1", "SPACEMAN_RUN_ID=run_1"}
	got := scrubEnv(in)
	want := []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/x", "CODEX_HOME=/y", "HC_TERMINAL_ID=t1", "SPACEMAN_RUN_ID=run_1"}
	if !slices.Equal(got, want) {
		t.Fatalf("scrubbed %v", got)
	}
}

// Merging keeps what the harness wrote and adds trust for another directory without dropping the first.
func TestMergeJSONFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude", ".claude.json") // the config dir does not exist yet
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(MergeJSONFile(p, map[string]any{"hasCompletedOnboarding": true, "projects": map[string]any{"/a": map[string]any{"hasTrustDialogAccepted": true}}}))
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
	b, _ := os.ReadFile(p)
	var doc map[string]any
	must(json.Unmarshal(b, &doc))
	doc["userID"] = "u"
	doc["projects"].(map[string]any)["/a"].(map[string]any)["allowedTools"] = []any{}
	b, _ = json.Marshal(doc)
	must(os.WriteFile(p, b, 0o600))
	must(MergeJSONFile(p, map[string]any{"hasCompletedOnboarding": true, "projects": map[string]any{"/b": map[string]any{"hasTrustDialogAccepted": true}}}))
	b, _ = os.ReadFile(p)
	doc = nil
	must(json.Unmarshal(b, &doc))
	projects := doc["projects"].(map[string]any)
	if doc["userID"] != "u" || projects["/a"].(map[string]any)["allowedTools"] == nil || projects["/b"].(map[string]any)["hasTrustDialogAccepted"] != true {
		t.Fatalf("merged %s", b)
	}
	must(os.WriteFile(p, []byte(`[1]`), 0o600))
	if err := MergeJSONFile(p, map[string]any{"x": 1}); err == nil {
		t.Fatal("a non-object file must be refused")
	}
}
