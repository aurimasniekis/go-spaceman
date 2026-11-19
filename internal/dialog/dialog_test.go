package dialog

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// memDir is the provisioned directory the add-dir fixture shows.
const memDir = "/srv/spaceman/memory/spc_example/worker/instances/ins_example"

func screen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func claude(v string) Context {
	return Context{Harness: "claude-code", Version: v, Dirs: []string{"/srv/work", memDir}, Home: "/home/agent"}
}

func TestReplayRecordedScreens(t *testing.T) {
	optIn := claude("2.1.328")
	optIn.CodeMode = true
	for _, tc := range []struct {
		name, file string
		c          Context
		kind       Kind
		id, signal string
		keys       []string
		answer     string
	}{
		{"theme picker: default", "claude-2.1.328-theme.txt", claude("2.1.328"), Known, "claude.theme_picker", SignalKnown, []string{"enter"}, "default"},
		{"add-dir trust: provisioned", "claude-2.1.328-add-dir.txt", claude("2.1.328"), Known, "claude.add_dir_trust", SignalKnown, []string{"down", "enter"}, "yes"},
		{"add-dir trust: not provisioned", "claude-2.1.328-add-dir.txt", Context{Harness: "claude-code", Version: "2.1.328", Dirs: []string{"/srv/work"}}, Unknown, "claude.add_dir_trust", SignalDeclined, nil, ""},
		{"add-dir trust: too old to know", "claude-2.1.328-add-dir.txt", claude("2.1.316"), Unknown, UnknownID, SignalPrompt, nil, ""},
		{"code mode: not opted in", "claude-2.1.328-code-mode.txt", claude("2.1.328"), Known, "claude.code_mode_optin", SignalKnown, []string{"enter"}, "no"},
		{"code mode: opted in", "claude-2.1.328-code-mode.txt", optIn, Known, "claude.code_mode_optin", SignalKnown, []string{"down", "enter"}, "yes"},
		{"code mode: other harness", "claude-2.1.328-code-mode.txt", Context{Harness: "codex", Version: "2.1.328"}, Unknown, UnknownID, SignalPrompt, nil, ""},
		{"login: never", "claude-2.1.328-login.txt", claude("2.1.328"), Unknown, UnknownID, SignalNever, nil, ""},
		{"permission: never", "claude-permission.txt", claude("2.1.328"), Unknown, UnknownID, SignalNever, nil, ""},
		{"unknown dialog", "claude-unknown.txt", claude("9.9.9"), Unknown, UnknownID, SignalPrompt, nil, ""},
		{"main prompt", "claude-2.1.328-main-idle.txt", claude("2.1.328"), None, "", "", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Classify(screen(t, tc.file), tc.c)
			if r.Kind != tc.kind || r.ID != tc.id || r.Signal != tc.signal || !slices.Equal(r.Keys, tc.keys) || r.Answer != tc.answer {
				t.Fatalf("got kind %d id %q signal %q keys %v answer %q, want %d %q %q %v %q", r.Kind, r.ID, r.Signal, r.Keys, r.Answer,
					tc.kind, tc.id, tc.signal, tc.keys, tc.answer)
			}
			if r.Kind != None && len(r.Lines) == 0 {
				t.Fatal("a dialog has no lines for its excerpt")
			}
		})
	}
}

// No registry entry may answer a permission or tool-approval dialog, even one whose text also matches the entry.
func TestNeverAnswersPermissionOrToolApproval(t *testing.T) {
	perm := screen(t, "claude-permission.txt")
	// An entry that would match anything on the permission screen and answer Yes.
	greedy := []Entry{{ID: "test.greedy", Harness: "claude-code", Text: []string{"Do you want to proceed?"},
		Answer: func(d Dialog, _ Context) (Answer, bool) { k, ok := d.Select("Yes"); return Answer{Choice: "yes", Keys: k}, ok }}}
	if r := classify(greedy, perm, claude("2.1.328")); r.Kind != Unknown || r.Signal != SignalNever || len(r.Keys) != 0 {
		t.Fatalf("permission dialog: %+v", r)
	}
	for _, body := range []string{
		" Do you want to make this edit to main.go?",
		" Allow once / Allow always",
		" Tool use requires approval",
		" Enter your API key",
		" Select login method:",
		" Do you want to allow Claude to fetch this content?",
	} {
		s := "────────────────────────────────────────\n" + body + "\n ❯ 1. Yes\n   2. No\n\n Enter to confirm · Esc to cancel\n"
		greedy[0].Text = []string{strings.TrimSpace(body)}
		if r := classify(greedy, s, claude("2.1.328")); r.Kind != Unknown || r.Signal != SignalNever || len(r.Keys) != 0 {
			t.Fatalf("%q: %+v", body, r)
		}
	}
}

// Text above the dialog's rule (the program's banner, startup notices) is not the dialog: a notice that mentions
// permissions does not put a startup dialog on the never-answer list.
func TestNeverListReadsOnlyTheDialog(t *testing.T) {
	s := " Auto mode lets Claude handle permission prompts automatically.\n" + screen(t, "claude-2.1.328-code-mode.txt")
	if r := Classify(s, claude("2.1.328")); r.Kind != Known || r.ID != "claude.code_mode_optin" {
		t.Fatalf("got %+v", r)
	}
}

func TestAddDirPathWrapped(t *testing.T) {
	long := "/srv/spaceman/memory/spc_example/a-role-with-a-rather-long-name/instances/ins_example_long_instance_name"
	s := strings.Replace(screen(t, "claude-2.1.328-add-dir.txt"), memDir, long[:40]+"\n "+long[40:], 1)
	c := claude("2.1.328")
	c.Dirs = []string{long}
	if r := Classify(s, c); r.Kind != Known || r.Answer != "yes" {
		t.Fatalf("wrapped path: %+v", r)
	}
	// A path that only starts like a provisioned one is not accepted.
	c.Dirs = []string{long[:40]}
	if r := Classify(s, c); r.Kind != Unknown || r.Signal != SignalDeclined {
		t.Fatalf("prefix path: %+v", r)
	}
	// "~/" is expanded with the run's home only.
	s2 := strings.Replace(screen(t, "claude-2.1.328-add-dir.txt"), memDir, "~/memory", 1)
	c = claude("2.1.328")
	c.Dirs = []string{"/home/agent/memory"}
	if r := Classify(s2, c); r.Kind != Known {
		t.Fatalf("home path: %+v", r)
	}
	c.Home = ""
	if r := Classify(s2, c); r.Kind != Unknown {
		t.Fatalf("home path without a home: %+v", r)
	}
}

func TestPlainPromptIsNotADialog(t *testing.T) {
	for _, s := range []string{
		"────────\n❯ \n────────\n",
		"────────\n❯ fix the failing test in the parser\n────────\n  ⏵⏵ auto mode on\n",
		"some output\n> not a cursor\n",
	} {
		if r := Classify(s, claude("2.1.328")); r.Kind != None {
			t.Fatalf("%q: %+v", s, r)
		}
	}
}

func TestSelectMovesFromTheCursor(t *testing.T) {
	d, ok := Find("────────────────────────\n Pick\n   1. One\n ❯ 2. Two\n   3. Three\n\n Enter to confirm\n")
	if !ok || d.Cursor != 1 || !slices.Equal(d.Options, []string{"One", "Two", "Three"}) {
		t.Fatalf("find: %+v %v", d, ok)
	}
	for prefix, want := range map[string][]string{"One": {"up", "enter"}, "Two": {"enter"}, "Three": {"down", "enter"}} {
		if k, ok := d.Select(prefix); !ok || !slices.Equal(k, want) {
			t.Fatalf("%s: %v", prefix, k)
		}
	}
	if _, ok := d.Select("Four"); ok {
		t.Fatal("selected a missing option")
	}
}
