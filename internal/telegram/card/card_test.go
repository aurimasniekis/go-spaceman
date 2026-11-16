package card

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

var update = flag.Bool("update", false, "rewrite testdata/*.json")

const (
	space  = "spc_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	action = "hum_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	appURL = "https://tg.example.com/spaces/x/human?action=y"
)

func question() View {
	return View{Label: "Question", Title: "Deploy the release candidate?", SpaceID: space, Space: "Alpha", Priority: "high", Full: true,
		Body: "The **build** is green.", Action: &Action{ID: action, Type: "question", Requester: "control", Options: []string{"Yes", "No"}, AllowText: true, Status: "open"}}
}

func approval(enforced bool) View {
	return View{Label: "Approval", Title: "Create the issue", SpaceID: space, Space: "Alpha", Priority: "normal", Full: true, Body: "args: secret-ish",
		Action: &Action{ID: action, Type: "approval", Requester: "worker", Enforced: enforced, Status: "open"}}
}

var all = Caps{Respond: true, Decide: true, AppURL: appURL}

type goldenCase struct {
	name string
	v    View
	c    Caps
}

func goldenCases() []goldenCase {
	q := question()
	qSpace := question()
	qSpace.ShowSpace = true
	qSummary := question()
	qSummary.Full = false
	qMany := question()
	qMany.Action.Options = []string{"a", "b", "c", "d", "e", "f", "g"}
	qOdd := question()
	qOdd.Action.Options, qOdd.Action.AllowText = []string{"red", "green", "blue"}, false
	qMeta := question()
	qMeta.Action.Reminders, qMeta.Action.Escalated, qMeta.Action.Claimed, qMeta.Action.Status = 2, true, "you", "in_progress"
	todo := View{Label: "To-do", Title: "Rotate the key", SpaceID: space, Priority: "low", Full: true, Action: &Action{ID: action, Type: "todo", Status: "open"}}
	ack := View{Label: "Message", Title: "Heads up", SpaceID: space, Full: true, Body: "Read me.", Action: &Action{ID: action, Type: "message", RequiresAck: true, Status: "open"}}
	info := ack
	info.Action = &Action{ID: action, Type: "message", Status: "open"}
	custom := View{Label: "release-gate", Title: "Gate the release", SpaceID: space, Full: true, Action: &Action{ID: action, Type: "custom", Status: "open"}}
	final := approval(false)
	final.Final, final.Action.Status, final.Action.Outcome = true, "resolved", "approved"
	cancelled := question()
	cancelled.Final, cancelled.Action.Status = true, "cancelled"
	inbox := View{Label: "Inbox", Title: "A note from the web", SpaceID: space, Space: "Alpha", ShowSpace: true, State: "new", Full: true, Body: "Hello _there_."}
	run := View{Label: "Run", Title: "worker", SpaceID: space, State: "failed", Full: true}
	long := question()
	long.Body = strings.Repeat("A paragraph of the body that goes on for a while.\n\n", 200)
	return []goldenCase{
		{"question", q, all},
		{"question_space", qSpace, all},
		{"question_summary", qSummary, all},
		{"question_many_options", qMany, all},
		{"question_odd_options", qOdd, all},
		{"question_meta", qMeta, all},
		{"question_no_respond", q, Caps{AppURL: appURL}},
		{"question_no_app", q, Caps{Respond: true, Decide: true}},
		{"approval", approval(false), all},
		{"approval_no_decide", approval(false), Caps{Respond: true, AppURL: appURL}},
		{"approval_enforced_web_only", approval(true), all},
		{"approval_enforced_web_only_no_app", approval(true), Caps{Respond: true, Decide: true}},
		{"approval_enforced_confirm", approval(true), Caps{Respond: true, Decide: true, EnforcedInChat: true, AppURL: appURL}},
		{"todo", todo, all},
		{"message_ack", ack, all},
		{"message_info", info, all},
		{"custom", custom, all},
		{"final_approved", final, all},
		{"final_cancelled", cancelled, all},
		{"inbox", inbox, Caps{AppURL: appURL}},
		{"run", run, Caps{}},
		{"long_body", long, all},
	}
}

type goldenPart struct {
	Text     string                          `json:"text"`
	Entities []botapi.MessageEntity          `json:"entities,omitempty"`
	// Markup is what an edit sends (an empty keyboard removes it); a send leaves an empty one out.
	Markup *botapi.InlineKeyboardMarkup `json:"markup"`
}

// TestGoldens renders each case and compares it with testdata/<case>.json. Run with -update to rewrite them, then
// review the diff.
func TestGoldens(t *testing.T) {
	for _, gc := range goldenCases() {
		t.Run(gc.name, func(t *testing.T) {
			parts := Render(gc.v, gc.c)
			var out []goldenPart
			for _, p := range parts {
				validate(t, p)
				out = append(out, goldenPart{Text: p.Text, Entities: p.Entities, Markup: p.EditMarkup()})
			}
			got, err := json.MarshalIndent(out, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			f := filepath.Join("testdata", gc.name+".json")
			if *update {
				if err := os.WriteFile(f, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("%v (run with -update)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden:\n%s", gc.name, got)
			}
		})
	}
}

// validate checks what Telegram would refuse: a part over the limit, an entity outside the text, callback data over
// 64 bytes, a button without text or with more than one action.
func validate(t *testing.T, p Part) {
	t.Helper()
	if n := Units(p.Text); n == 0 || n > MaxMessage {
		t.Errorf("part of %d units", n)
	}
	for _, e := range p.Entities {
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > Units(p.Text) {
			t.Errorf("entity %+v outside a text of %d units", e, Units(p.Text))
		}
	}
	for _, row := range p.Keyboard {
		for _, b := range row {
			if b.Text == "" || len(b.CallbackData) > 64 || (b.CallbackData == "") == (b.WebApp == nil) {
				t.Errorf("bad button %+v", b)
			}
		}
	}
}

// A card's body is shown only at content level full, and an enforced approval's never, whatever the caller passed.
func TestBodyOnlyWhenAllowed(t *testing.T) {
	q := question()
	q.Full = false
	q.Body = "TOPSECRET"
	for _, p := range Render(q, all) {
		if strings.Contains(p.Text, "TOPSECRET") {
			t.Fatalf("summary card shows the body: %q", p.Text)
		}
	}
	ap := approval(true)
	ap.Body = "TOPSECRET"
	for _, c := range []Caps{all, {Respond: true, Decide: true, EnforcedInChat: true}} {
		for _, p := range Render(ap, c) {
			if strings.Contains(p.Text, "TOPSECRET") {
				t.Fatalf("enforced approval shows its body: %q", p.Text)
			}
		}
	}
	fin := question()
	fin.Final, fin.Body = true, "TOPSECRET"
	if p := Render(fin, all); len(p) != 1 || strings.Contains(p[0].Text, "TOPSECRET") || p[0].Markup() != nil {
		t.Fatalf("final card: %+v", p)
	}
}

// An enforced approval in a space that decides them on the web offers no callback at all: not Approve, not Deny, not
// the form.
func TestEnforcedWebOnlyHasNoCallbacks(t *testing.T) {
	for _, c := range []Caps{all, {Respond: true, Decide: true}} {
		for _, row := range Keyboard(approval(true), c) {
			for _, b := range row {
				if b.CallbackData != "" {
					t.Fatalf("callback %q on an enforced web-only approval", b.CallbackData)
				}
			}
		}
	}
}

// Every callback a card carries names its space and action, so a tap never depends on a mapping.
func TestCallbackDataNamesTheAction(t *testing.T) {
	for _, gc := range goldenCases() {
		for _, row := range Keyboard(gc.v, gc.c) {
			for _, b := range row {
				if b.CallbackData == "" {
					continue
				}
				f := strings.Split(b.CallbackData, ":")
				if len(f) < 3 || "spc_"+f[1] != space || "hum_"+f[2] != action {
					t.Errorf("%s: %q", gc.name, b.CallbackData)
				}
			}
		}
	}
}

func TestClean(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"a\u202eb\x00c", 10, "a b c"},
		{"  two   words ", 10, "two words"},
		{"abcdef", 4, "abcd…"},
		{"abcd", 4, "abcd"},
		{"ab cd", 4, "ab c…"},
	} {
		if got := Clean(c.in, c.n); got != c.want {
			t.Errorf("Clean(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}
