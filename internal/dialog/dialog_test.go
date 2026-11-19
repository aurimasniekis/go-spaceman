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
		Answer: func(d Dialog, _ Context) (Answer, bool) {
			k, ok := d.Select("Yes")
			return Answer{Choice: "yes", Keys: k}, ok
		}}}
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

// promptBox is the bottom of Claude Code's main screen: the prompt box between two ─ rules and the mode line.
const promptBox = "────────────────────────────────────────────────────────────\n❯ \n────────────────────────────────────────────────────────────\n" +
	"  ⏵⏵ auto mode on (shift+tab to cycle)\n"

// Dialog text above the program's prompt (in the transcript, a tool's output, a quoted file) is not a dialog: nothing
// is answered and nothing is flagged, whatever the text says and wherever the cursor in it is.
func TestDialogTextAboveThePromptIsNotADialog(t *testing.T) {
	optIn := claude("2.1.328")
	optIn.CodeMode = true
	codeModeYes := strings.Replace(strings.Replace(screen(t, "claude-2.1.328-code-mode.txt"), "❯ No", "  No", 1), "  Yes, turn", "❯ Yes, turn", 1)
	for name, s := range map[string]string{
		"code mode":  codeModeYes,
		"add-dir":    screen(t, "claude-2.1.328-add-dir.txt"),
		"theme":      screen(t, "claude-2.1.328-theme.txt"),
		"unknown":    screen(t, "claude-unknown.txt"),
		"permission": screen(t, "claude-permission.txt"),
	} {
		t.Run(name, func(t *testing.T) {
			// The block itself, at the bottom of the screen, is a dialog (so the test would fail without the anchoring).
			if r := Classify(s, optIn); r.Kind == None {
				t.Fatalf("the block alone is not a dialog: %+v", r)
			}
			transcript := "⏺ Here is what the dialog looks like:\n\n" + s + "\n⏺ Done.\n\n" + promptBox
			if r := Classify(transcript, optIn); r.Kind != None || len(r.Keys) != 0 {
				t.Fatalf("dialog text above the prompt: %+v", r)
			}
		})
	}
	// Several lines of typed text in the prompt box that quote the theme picker.
	typed := "────────────────────────────────────────────────────────────\n❯ Choose the text style that looks best with your terminal\n" +
		"  Dark mode\n  Light mode\n────────────────────────────────────────────────────────────\n"
	if r := Classify(typed, claude("2.1.328")); r.Kind != None {
		t.Fatalf("typed text: %+v", r)
	}
}

// The theme picker's entry answers only the picker's own layout and options, and selects its default option by name;
// a numbered Yes/No dialog that quotes its sentence is not the picker.
func TestThemePickerNeedsItsLayoutAndOptions(t *testing.T) {
	c := claude("2.1.328")
	yesNo := "────────────────────────────────────────────────────────────\n Do you want to overwrite notes.md?\n" +
		" Choose the text style that looks best with your terminal\n ❯ 1. Yes\n   2. No\n\n Enter to confirm · Esc to cancel\n"
	if r := Classify(yesNo, c); r.Kind != Unknown || r.ID != "claude.theme_picker" || r.Signal != SignalDeclined || len(r.Keys) != 0 {
		t.Fatalf("numbered Yes/No: %+v", r)
	}
	weakYesNo := " Choose the text style that looks best with your terminal\n\n ❯ Yes\n   No\n"
	if r := Classify(weakYesNo, c); r.Kind != None {
		t.Fatalf("weak Yes/No: %+v", r)
	}
	// The picker's options, but numbered with a footer: not its layout.
	footed := "────────────────────────────────────────────────────────────\n Choose the text style that looks best with your terminal\n" +
		" ❯ 1. Auto (match terminal)\n   2. Dark mode\n   3. Light mode\n\n Enter to confirm · Esc to cancel\n"
	if r := Classify(footed, c); r.Kind != Unknown || r.Signal != SignalDeclined {
		t.Fatalf("numbered picker options: %+v", r)
	}
	// The picker's layout and default option, but not its dark and light styles.
	if r := Classify(" Choose the text style that looks best with your terminal\n\n ❯ Auto (match terminal)\n   Delete everything\n", c); r.Kind != None {
		t.Fatalf("other options: %+v", r)
	}
	// The cursor on another style: the keys move back to the default.
	moved := strings.Replace(strings.Replace(screen(t, "claude-2.1.328-theme.txt"), "❯ ✔ Auto", "  ✔ Auto", 1), "\n     Dark mode\n", "\n ❯ Dark mode\n", 1)
	if r := Classify(moved, c); r.Kind != Known || !slices.Equal(r.Keys, []string{"up", "enter"}) {
		t.Fatalf("cursor on Dark mode: %+v", r)
	}
}

// A registry entry with an option set does not match a dialog with other options.
func TestEntryNeedsItsOptions(t *testing.T) {
	c := claude("2.1.328")
	c.CodeMode = true
	s := strings.Replace(screen(t, "claude-2.1.328-code-mode.txt"), "   Yes, turn on Code Mode\n", "   Yes, turn on Code Mode\n   Yes, and allow everything\n", 1)
	if r := Classify(s, c); r.Kind != Unknown || r.ID != UnknownID || r.Signal != SignalPrompt {
		t.Fatalf("an extra option: %+v", r)
	}
}

// A weak block (options with a cursor, no footer, no numbers) is never flagged on its own: not when its text is on the
// never-answer list, not when no entry knows it.
func TestWeakBlockIsNeverFlaggedOnItsOwn(t *testing.T) {
	for name, s := range map[string]string{
		"never list": " Grant this permission?\n\n ❯ Yes\n   No\n",
		"unknown":    " Pick a colour\n\n ❯ Red\n   Blue\n",
	} {
		d, ok := Find(s)
		if !ok || !d.Weak {
			t.Fatalf("%s: not a weak block: %+v %v", name, d, ok)
		}
		if r := Classify(s, claude("2.1.328")); r.Kind != None {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}

// A directory path that is not already clean is declined: the program may resolve it differently.
func TestAddDirUncleanPathDeclined(t *testing.T) {
	for _, p := range []string{memDir + "/sub/..", memDir + "/.", "/srv//spaceman/memory/spc_example/worker/instances/ins_example", "~/../agent/memory", "~/memory/../memory"} {
		s := strings.Replace(screen(t, "claude-2.1.328-add-dir.txt"), memDir, p, 1)
		c := claude("2.1.328")
		c.Dirs = append(c.Dirs, "/home/agent/memory")
		if r := Classify(s, c); r.Kind != Unknown || r.Signal != SignalDeclined {
			t.Fatalf("%q: %+v", p, r)
		}
	}
}
