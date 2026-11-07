package app_test

import (
	"context"
	"fmt"
	"testing"
)

// ADR-0059 over HTTP: a claude-code profile takes any settings.json keys, an update that leaves them out keeps them (an
// older client does not wipe them), an empty object clears them, a codex profile is refused them, and a malformed key
// SpaceMan edits is refused.
func TestClaudeSettingsOverHTTP(t *testing.T) {
	h := newHarness(t)
	secret, _, _ := h.c.Auth.IssueRootToken(context.Background())
	root := h.client()
	root.bearer = secret

	type profile struct {
		ID             string
		Version        int64
		ClaudeSettings map[string]any `json:"claude_settings"`
	}
	settings := map[string]any{"autoMode": map[string]any{"allow": []string{"$defaults", "Pushing to example.com"}},
		"permissions": map[string]any{"deny": []string{"Read(./secrets/**)"}}, "statusLine": map[string]any{"type": "command", "command": "echo run"}}
	allow := func(p profile) int {
		a, _ := p.ClaudeSettings["autoMode"].(map[string]any)
		l, _ := a["allow"].([]any)
		return len(l)
	}
	var p profile
	r := root.do("PUT", "/harness-profiles/new", map[string]any{"slug": "tf-claude", "name": "Claude", "harness": "claude-code", "claude_settings": settings})
	expect(t, r, 200)
	r.json(t, &p)
	if allow(p) != 2 || p.ClaudeSettings["statusLine"] == nil {
		t.Fatalf("created %+v", p.ClaudeSettings)
	}
	r = root.do("PUT", "/harness-profiles/"+p.ID, map[string]any{"name": "Claude renamed"}, "If-Match", fmt.Sprintf(`"%d"`, p.Version))
	expect(t, r, 200)
	r.json(t, &p)
	if allow(p) != 2 {
		t.Fatalf("settings left out of an update were not kept: %+v", p.ClaudeSettings)
	}
	r = root.do("PUT", "/harness-profiles/"+p.ID, map[string]any{"name": "Claude", "claude_settings": map[string]any{}}, "If-Match", fmt.Sprintf(`"%d"`, p.Version))
	expect(t, r, 200)
	p = profile{} // decoding into the old map would keep its keys
	r.json(t, &p)
	if len(p.ClaudeSettings) != 0 {
		t.Fatalf("not cleared: %+v", p.ClaudeSettings)
	}
	expect(t, root.do("PUT", "/harness-profiles/new", map[string]any{"slug": "tf-codex", "name": "Codex", "harness": "codex", "claude_settings": settings}), 422)
	expect(t, root.do("PUT", "/harness-profiles/new", map[string]any{"slug": "tf-bad", "name": "Bad", "harness": "claude-code",
		"claude_settings": map[string]any{"permissions": map[string]any{"defaultMode": "yolo"}}}), 422)
}
