// Package card renders a Telegram card (ADR-0089, plan §2.6): the text, entities and inline keyboard of one item, from
// a view of the item and what the link may do. It is pure (no I/O, no clock, no state): the notifier renders its cards
// with it (notify/cards.go), and the chat package renders a card again with it after a form opened from the card goes
// back to it (chat/needsyou.go), so both draw the same message the same way.
//
// The keyboard is a convenience only. A button that is shown is no permission and a button that is hidden is no
// protection: every tap is authorized again as the tapping link by the API (the chat package's humanCallback), and an
// enforced approval is decided only as the space allows it (api telegram_approvals.go). Caps only decides what is
// drawn.
package card

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/render"
)

// Verbs of the buttons a card carries. Callback data is "h<verb>:<space>:<action>[:<n>]" with the IDs' prefixes left
// out (at most 64 bytes: "hq:" + 26 + ":" + 26 + ":" + n). The chat package owns the whole verb list (its humanData);
// these are the ones on a card, and the two that move between a card and its form.
const (
	VerbOption   = 'q' // a question's option n
	VerbText     = 'f' // a question's free-text answer (asks with a ForceReply)
	VerbComplete = 'c' // complete a to-do
	VerbAck      = 'm' // acknowledge or dismiss a message
	VerbApprove  = 'a' // approve (asks to confirm first)
	VerbDeny     = 'd' // deny (asks for a note)
	VerbMore     = 'n' // the full form, in place of the card
	VerbCard     = 'z' // back from the form to the card
)

// Limits.
const (
	// MaxQuick is the most options of a question a card offers as buttons; a question with more is answered from the
	// form (More…).
	MaxQuick = 6
	// MaxTitle is the longest title shown, in runes.
	MaxTitle = 200
	// MaxMessage is a message's length limit in UTF-16 units (the Bot API's).
	MaxMessage = 4096
	// MaxParts is the most messages a card's body is split into (the Mini App has the rest).
	MaxParts = 4
	contMark = "(%d/%d)\n"
	// Gone is the whole text of a card the link may no longer see (lost access, a muted or a hidden space): nothing of
	// the item is shown.
	Gone = "No longer available here."
	// TextSummary ends a card whose space shows no bodies in chat.
	TextSummary = "Open the app for details."
)

// Action is what a human action's card needs beyond the common view.
type Action struct {
	ID          string
	Type        string // question, approval, todo, message, custom
	Requester   string
	Options     []string // a question's options
	AllowText   bool     // a question takes a typed answer
	RequiresAck bool     // a message asks to be acknowledged
	// Enforced: an approval with an enforced approval behind it (or one that can't be shown to be advisory). Its body
	// is never rendered (SM-APPR-005).
	Enforced bool
	Status   string // open, in_progress, resolved, cancelled, expired
	Outcome  string // approved, denied, answered, completed, acknowledged, a custom outcome
	// Reminders is how many reminders were sent; Escalated says it was escalated.
	Reminders int64
	Escalated bool
	// Claimed is who holds it, as the viewer sees it: "you", "someone else" or "" (nobody).
	Claimed string
}

// View is one item as a card shows it.
type View struct {
	// Label is the item's kind ("Question", "Inbox", "Task").
	Label string
	Title string
	// SpaceID is the item's space; Space is its name, shown in the header when ShowSpace (outside the space's topic).
	SpaceID   string
	Space     string
	ShowSpace bool
	// State is the status line of an item that is not a human action ("new", "Unread").
	State    string
	Priority string
	// Body is the item's Markdown body, rendered only when Full (the space's content level allows it in this chat).
	Body string
	Full bool
	// Final: the item is settled; the card becomes one compact line without a keyboard.
	Final bool
	// Action is set for a human action.
	Action *Action
}

// Caps is what the card offers the link it is shown to. Cosmetic only (see the package comment).
type Caps struct {
	// Respond: the link may respond to the action (human_action:respond); Decide: it may decide approvals
	// (approval:decide).
	Respond, Decide bool
	// EnforcedInChat: the space lets enforced approvals be decided in chat (enforced_approvals = confirm).
	EnforcedInChat bool
	// AppURL is the item's Mini App URL, for "Open in app" and for a body cut at the message cap ("": none; always
	// "" in a group, where web_app buttons are refused).
	AppURL string
}

// Part is one message of a card.
type Part struct {
	Text     string
	Entities []botapi.MessageEntity
	// Keyboard is the card's buttons (the first part only).
	Keyboard [][]botapi.InlineKeyboardButton
	// Buttons are the body's own Mini App buttons (a cut body's "continue" link), on the part they belong to.
	Buttons []render.Button
}

// Markup is the part's inline keyboard: its keyboard rows, then the body's buttons in one row (nil: none).
func (p Part) Markup() *botapi.InlineKeyboardMarkup {
	rows := append([][]botapi.InlineKeyboardButton(nil), p.Keyboard...)
	if len(p.Buttons) > 0 {
		row := make([]botapi.InlineKeyboardButton, len(p.Buttons))
		for i, b := range p.Buttons {
			row[i] = botapi.InlineKeyboardButton{Text: b.Text, WebApp: &botapi.WebAppInfo{URL: b.WebAppURL}}
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	return &botapi.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// EditMarkup is the keyboard an edit carries: never nil, so an edit always states the keyboard (an empty one removes
// it) instead of relying on Telegram dropping it from an edit without one.
func (p Part) EditMarkup() *botapi.InlineKeyboardMarkup {
	if m := p.Markup(); m != nil {
		return m
	}
	return &botapi.InlineKeyboardMarkup{InlineKeyboard: [][]botapi.InlineKeyboardButton{}}
}

// Data is a button's callback data: "h<verb>:<space>:<action>[:<n>]", the IDs without their "spc_" and "hum_"
// prefixes; n < 0 is left out.
func Data(verb byte, spaceID, actionID string, n int) string {
	d := fmt.Sprintf("h%c:%s:%s", verb, strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(actionID, "hum_"))
	if n >= 0 {
		d += ":" + strconv.Itoa(n)
	}
	return d
}

// Label is a human action's kind as a card names it: the built-in types by name, a custom type by its slug.
func Label(actionType, slug string) string {
	switch actionType {
	case "question":
		return "Question"
	case "approval":
		return "Approval"
	case "todo":
		return "To-do"
	case "message":
		return "Message"
	}
	if slug != "" {
		return slug
	}
	return "Action"
}

// Mark is the type's sign in front of the label.
func Mark(label, actionType string) string {
	switch actionType {
	case "question":
		return "❓"
	case "approval":
		return "🔐"
	case "todo":
		return "☑"
	case "message":
		return "✉"
	case "custom":
		return "🧩"
	}
	switch label {
	case "Inbox":
		return "📥"
	case "Task":
		return "📋"
	case "Message":
		return "💬"
	case "Run":
		return "⚠"
	}
	return "•"
}

// Head is a card's header: the sign, label and (outside the space's topic) the space's name; the title in bold; and
// the meta line. The chat's form uses it above its controls.
func Head(v View) (string, []botapi.MessageEntity) {
	var b strings.Builder
	typ := ""
	if v.Action != nil {
		typ = v.Action.Type
	}
	b.WriteString(Mark(v.Label, typ) + " " + Clean(v.Label, 40))
	if v.ShowSpace && Clean(v.Space, 80) != "" {
		b.WriteString(" · " + Clean(v.Space, 80))
	}
	b.WriteString("\n")
	title := Clean(v.Title, MaxTitle)
	if title == "" {
		title = "(untitled)"
	}
	off := Units(b.String())
	b.WriteString(title)
	ents := []botapi.MessageEntity{{Type: "bold", Offset: off, Length: Units(title)}}
	if m := meta(v); m != "" {
		b.WriteString("\n" + m)
	}
	return b.String(), ents
}

// meta is the line under the title.
func meta(v View) string {
	var xs []string
	a := v.Action
	if a == nil {
		if s := Clean(v.State, 40); s != "" {
			xs = append(xs, s)
		}
		return strings.Join(xs, " · ")
	}
	if p := Clean(v.Priority, 20); p != "" {
		xs = append(xs, "Priority "+p)
	}
	if r := Clean(a.Requester, 60); r != "" {
		xs = append(xs, "from "+r)
	}
	switch a.Claimed {
	case "you":
		xs = append(xs, "claimed by you")
	case "":
	default:
		xs = append(xs, "claimed by someone else")
	}
	if a.Reminders > 0 {
		xs = append(xs, fmt.Sprintf("reminded %d×", a.Reminders))
	}
	if a.Escalated {
		xs = append(xs, "escalated")
	}
	if st := Clean(a.Status, 20); st != "" && st != "open" {
		xs = append(xs, strings.ReplaceAll(st, "_", " "))
	}
	return strings.Join(xs, " · ")
}

// Final is the compact line a settled card becomes: a sign, the outcome (or the status) and the title. No body and no
// keyboard.
func Final(v View) string {
	word := Clean(v.State, 40)
	if a := v.Action; a != nil {
		word = Clean(a.Outcome, 32)
		if word == "" {
			word = Clean(a.Status, 20)
		}
	}
	word = strings.ReplaceAll(word, "_", " ")
	sign := "✔"
	switch strings.ToLower(word) {
	case "approved", "answered", "completed", "acknowledged", "done", "resolved", "read":
		sign = "✅"
	case "denied", "rejected", "failed", "lost":
		sign = "❌"
	case "cancelled", "canceled":
		sign = "🗑"
	case "expired":
		sign = "⌛"
	}
	if word == "" {
		word = "Settled"
	}
	r, n := utf8.DecodeRuneInString(word)
	word = string(unicode.ToUpper(r)) + word[n:]
	title := Clean(v.Title, MaxTitle)
	if title == "" {
		return sign + " " + word
	}
	return sign + " " + word + " · " + title
}

// Render builds a card's messages. A settled card is one compact line. Otherwise the first message has the header, the
// start of the body (only when Full; never for an enforced approval) and the keyboard; a body that doesn't fit goes on
// in continuation messages ("(2/3)"), at most MaxParts in all. The body is Markdown turned into entities (never raw
// HTML; links only http, https and mailto).
func Render(v View, c Caps) []Part {
	if v.Final {
		return []Part{{Text: Final(v)}}
	}
	head, ents := Head(v)
	kb := Keyboard(v, c)
	body := v.Body
	if a := v.Action; a != nil && a.Type == "approval" && a.Enforced {
		body = "" // an enforced approval's body carries its arguments (SM-APPR-005), whatever the caller passed
	}
	if !v.Full {
		return []Part{{Text: head + "\n\n" + TextSummary, Entities: ents, Keyboard: kb}}
	}
	if strings.TrimSpace(body) == "" {
		return []Part{{Text: head, Entities: ents, Keyboard: kb}}
	}
	prefix := head + "\n\n"
	shift := Units(prefix)
	// Every part is cut to the same size, small enough for the first (the header before the body).
	msg := render.Parse(body).Entities(render.Options{Limit: MaxMessage - shift, MaxParts: MaxParts, ContinueURL: c.AppURL})
	if len(msg.Parts) == 0 {
		return []Part{{Text: head, Entities: ents, Keyboard: kb}}
	}
	first := msg.Parts[0]
	parts := []Part{{Text: prefix + first.Text, Entities: append(ents, entities(first.Entities, shift)...), Keyboard: kb, Buttons: first.Buttons}}
	for i, p := range msg.Parts[1:] {
		mark := fmt.Sprintf(contMark, i+2, len(msg.Parts))
		parts = append(parts, Part{Text: mark + p.Text, Entities: entities(p.Entities, Units(mark)), Buttons: p.Buttons})
	}
	return parts
}

// Keyboard is a live card's buttons: the type's quick actions, then More… (the full form in place) and Open in app.
// A settled card and anything but a human action have none, except Open in app.
func Keyboard(v View, c Caps) [][]botapi.InlineKeyboardButton {
	if v.Final {
		return nil
	}
	var app []botapi.InlineKeyboardButton
	if c.AppURL != "" {
		app = []botapi.InlineKeyboardButton{{Text: "Open in app", WebApp: &botapi.WebAppInfo{URL: c.AppURL}}}
	}
	a := v.Action
	if a == nil || a.ID == "" || v.SpaceID == "" {
		return rows(app)
	}
	data := func(verb byte, n int) string { return Data(verb, v.SpaceID, a.ID, n) }
	btn := func(text string, verb byte, n int) botapi.InlineKeyboardButton {
		return botapi.InlineKeyboardButton{Text: text, CallbackData: data(verb, n)}
	}
	if a.Type == "approval" && a.Enforced && !c.EnforcedInChat {
		// Decided in the browser only (ADR-0083 decision 14): nothing else, not even the form.
		if c.AppURL == "" {
			return nil
		}
		return [][]botapi.InlineKeyboardButton{{{Text: "Decide in app", WebApp: &botapi.WebAppInfo{URL: c.AppURL}}}}
	}
	if !c.Respond {
		return rows(app)
	}
	var out [][]botapi.InlineKeyboardButton
	more := "More…"
	switch a.Type {
	case "question":
		if len(a.Options) <= MaxQuick {
			var row []botapi.InlineKeyboardButton
			for i, o := range a.Options {
				label := Clean(o, 40)
				if label == "" {
					label = "—"
				}
				row = append(row, btn(label, VerbOption, i))
				if len(row) == 2 {
					out, row = append(out, row), nil
				}
			}
			if a.AllowText || len(a.Options) == 0 {
				row = append(row, btn("✏ Answer", VerbText, -1))
			}
			out = append(out, rows(row)...)
		}
	case "todo":
		out = append(out, []botapi.InlineKeyboardButton{btn("Mark completed", VerbComplete, -1)})
	case "message":
		label := "Dismiss"
		if a.RequiresAck {
			label = "Acknowledge"
		}
		out = append(out, []botapi.InlineKeyboardButton{btn(label, VerbAck, -1)})
	case "approval":
		if c.Decide {
			approve := btn("✅ Approve", VerbApprove, -1)
			approve.Style = botapi.StyleSuccess
			deny := btn("❌ Deny", VerbDeny, -1)
			deny.Style = botapi.StyleDanger
			out = append(out, []botapi.InlineKeyboardButton{approve, deny})
		}
	case "custom":
		more = "Open"
	}
	return append(out, append([]botapi.InlineKeyboardButton{btn(more, VerbMore, -1)}, app...))
}

func rows(row []botapi.InlineKeyboardButton) [][]botapi.InlineKeyboardButton {
	if len(row) == 0 {
		return nil
	}
	return [][]botapi.InlineKeyboardButton{row}
}

// Units is the length of s in UTF-16 code units, the measure of the Bot API's limits.
func Units(s string) int { return len(utf16.Encode([]rune(s))) }

func entities(es []render.Entity, shift int) []botapi.MessageEntity {
	out := make([]botapi.MessageEntity, len(es))
	for i, e := range es {
		out[i] = botapi.MessageEntity{Type: e.Type, Offset: e.Offset + shift, Length: e.Length, URL: e.URL, Language: e.Language}
	}
	return out
}

// Clean makes untrusted text safe for one line of plain text: control and format characters become spaces, runs of
// spaces collapse, and the result is cut to at most n runes (with an ellipsis).
func Clean(s string, n int) string {
	var b strings.Builder
	space := false
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if count >= n {
			return strings.TrimSpace(b.String()) + "…"
		}
		if space {
			b.WriteByte(' ')
			count++
			space = false
			if count >= n {
				return strings.TrimSpace(b.String()) + "…"
			}
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}
