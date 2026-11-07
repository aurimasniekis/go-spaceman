package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// SpaceMan's keys are set whole, keys it set earlier and no longer gives are removed, and keys Claude Code wrote itself
// stay (ADR-0059).
func TestOwnJSONKeysFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "claude", "settings.json")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	read := func() map[string]any {
		t.Helper()
		b, _ := os.ReadFile(p)
		var doc map[string]any
		must(json.Unmarshal(b, &doc))
		return doc
	}
	must(OwnJSONKeysFile(p, map[string]any{"autoMode": map[string]any{"allow": []string{"a", "b"}}, "hooks": map[string]any{"Stop": []any{}}}))
	doc := read()
	doc["model"] = "opus" // written by Claude Code itself
	b, _ := json.Marshal(doc)
	must(os.WriteFile(p, b, 0o600))
	must(OwnJSONKeysFile(p, map[string]any{"autoMode": map[string]any{"environment": []string{"e"}}}))
	doc = read()
	auto := doc["autoMode"].(map[string]any)
	if doc["model"] != "opus" || doc["hooks"] != nil || auto["allow"] != nil || len(auto["environment"].([]any)) != 1 {
		t.Fatalf("settings %v", doc)
	}
	must(OwnJSONKeysFile(p, map[string]any{}))
	if doc = read(); len(doc) != 1 || doc["model"] != "opus" {
		t.Fatalf("after clearing %v", doc)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", fi, err)
	}
}

// A corrupt keys record fails instead of guessing which keys SpaceMan set; the record is written before the settings,
// so a settings write that fails still leaves every key that may be in the settings recorded for the next launch.
func TestOwnJSONKeysFileRecord(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	record := p + ".spaceman-keys"
	if err := os.WriteFile(record, []byte(`{"hooks": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := OwnJSONKeysFile(p, map[string]any{"permissions": map[string]any{}}); err == nil || !strings.Contains(err.Error(), "not a JSON list") {
		t.Fatalf("corrupt record: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("settings written despite a corrupt record: %v", err)
	}
	if err := os.WriteFile(record, []byte(`["hooks"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`["not an object"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := OwnJSONKeysFile(p, map[string]any{"permissions": map[string]any{}}); err == nil {
		t.Fatal("a settings file that is not an object was accepted")
	}
	if b, _ := os.ReadFile(record); string(b) != `["hooks","permissions"]` {
		t.Fatalf("record after a failed settings write: %s", b)
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
