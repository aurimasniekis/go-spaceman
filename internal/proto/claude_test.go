package proto

import (
	"encoding/json"
	"strings"
	"testing"
)

// Claude Code settings are any settings.json keys; the keys SpaceMan edits are checked for shape, the size is bounded,
// and permissions.defaultMode can be read and replaced without changing the original (ADR-0059).
func TestClaudeSettingsValidate(t *testing.T) {
	var ok ClaudeSettings
	if err := json.Unmarshal([]byte(`{
	  "permissions": {"defaultMode": "auto", "allow": ["Bash(make test:*)"], "deny": ["Read(./secrets/**)"], "additionalDirectories": ["/data"]},
	  "autoMode": {"environment": ["$defaults", "Trusted hosts: example.com"], "soft_deny": ["Only this"]},
	  "env": {"FOO": "bar"},
	  "hooks": {"PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "true"}]}]},
	  "statusLine": {"type": "command", "command": "echo hi"}
	}`), &ok); err != nil {
		t.Fatal(err)
	}
	if p := ok.Validate(); len(p) != 0 {
		t.Fatalf("valid settings refused: %v", p)
	}
	for name, bad := range map[string]string{
		"permissions not an object": `{"permissions": ["x"]}`,
		"allow not strings":         `{"permissions": {"allow": [1]}}`,
		"unknown default mode":      `{"permissions": {"defaultMode": "yolo"}}`,
		"autoMode list":             `{"autoMode": {"environment": "one"}}`,
		"env value":                 `{"env": {"N": 1}}`,
		"too large":                 `{"x": "` + strings.Repeat("a", MaxClaudeSettingsBytes) + `"}`,
	} {
		var s ClaudeSettings
		if err := json.Unmarshal([]byte(bad), &s); err != nil {
			t.Fatal(name, err)
		}
		if p := s.Validate(); len(p) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if ok.DefaultMode() != "auto" || (ClaudeSettings{}).DefaultMode() != "" {
		t.Fatal("DefaultMode")
	}
	lowered := ok.WithDefaultMode("default")
	if lowered.DefaultMode() != "default" || ok.DefaultMode() != "auto" || len(lowered["permissions"].(map[string]any)["allow"].([]any)) != 1 {
		t.Fatalf("WithDefaultMode %v / %v", lowered, ok)
	}
	if k := ok.Keys(); strings.Join(k, ",") != "autoMode,env,hooks,permissions,statusLine" {
		t.Fatalf("keys %v", k)
	}
}
