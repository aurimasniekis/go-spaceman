package runner

import "testing"

// ADR-0100: Claude Code reports `working` with "… still running" after a finished turn while background tasks it
// started still run. Classify marks only that report KindBackground, which takes notices; nothing else changes.
func TestClassifyBackground(t *testing.T) {
	for _, tc := range []struct {
		in       Status
		kind     string
		takesNot bool
	}{
		{Status{State: "working", App: "claude-code", Message: "1 shell still running"}, KindBackground, true},
		{Status{State: "working", App: "claude-code", Message: "2 shells, 1 monitor still running"}, KindBackground, true},
		{Status{State: "working", App: "claude-code", Message: "1 background agent still running"}, KindBackground, true},
		{Status{State: "working", App: "claude-code", Message: "Reading files"}, "", false},
		{Status{State: "working", App: "claude-code"}, "", false},
		{Status{State: "working", App: "codex", Message: "1 shell still running"}, "", false},
		{Status{State: "blocked", App: "claude-code", Kind: "permission", Message: "1 shell still running"}, "permission", false},
		{Status{State: "done", App: "claude-code", Message: "1 shell still running"}, "", true},
		{Status{State: "idle"}, "", true},
		{Status{State: "error", App: "claude-code"}, "", false},
		{Status{}, "", false},
	} {
		got := Classify(tc.in)
		if got.Kind != tc.kind || got.State != tc.in.State || got.Message != tc.in.Message || got.TakesNotice() != tc.takesNot {
			t.Errorf("Classify(%+v) = %+v (takes notice %v), want kind %q, takes notice %v", tc.in, got, got.TakesNotice(), tc.kind, tc.takesNot)
		}
	}
}
