package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Needs-you forms (ADR-0083, plan §2.4, TG-7-1): the human actions waiting for a person, answered from the chat.
// Questions (options, free text), todos (step toggles, complete), messages (acknowledge) and custom types with a simple
// response schema (a wizard) are handled here. An approval is NOT: its card says to open it in the app (TG-7-2).
//
// Like everything in this package, a tap acts as the link's principal through the in-process API and responds with the
// version the person saw (If-Match). A form is bound to (chat, form message, link); a free-text reply is bound to
// (chat, prompt message, link) with a TTL. Taps whose form is unknown or expired never respond: the form is shown again
// so the person sees the current state first.

var humanIDRe = regexp.MustCompile(`^hum_[0-9a-z]{26}$`)

// Texts of the needs-you forms.
const (
	TextFormExpired   = "That form expired. Open it again from /needsyou."
	TextApprovalInApp = "Approvals are decided in the app."
	TextOpenInApp     = "Open it in the app to answer."
	maxWizardFields   = 8
	maxWizardChoices  = 8
	maxFormsPerLink   = 10
	bodyRunes         = 1500
	maxOptions        = 20
)

// humanAction is the part of an action the forms read.
type humanAction struct {
	ID             string          `json:"id"`
	SpaceID        string          `json:"space_id"`
	Type           string          `json:"type"`
	TypeSlug       string          `json:"type_slug"`
	Title          string          `json:"title"`
	Body           string          `json:"body"`
	Request        json.RawMessage `json:"request"`
	RequesterLabel string          `json:"requester_label"`
	Priority       string          `json:"priority"`
	Status         string          `json:"status"`
	Outcome        string          `json:"outcome"`
	Version        int64           `json:"version"`
	TypeSpec       *struct {
		Outcomes       []string        `json:"outcomes"`
		ResponseSchema json.RawMessage `json:"response_schema"`
		UI             json.RawMessage `json:"ui"`
	} `json:"type_spec"`
}

func (a humanAction) pending() bool { return a.Status == "open" || a.Status == "in_progress" }

// wfield is one field of a custom type's wizard.
type wfield struct {
	name, label string
	kind        string // string, number, integer, boolean
	choices     []string
	required    bool
}

type wizard struct {
	fields   []wfield
	idx      int
	values   map[string]any
	outcomes []string
}

// form is a needs-you form on display: what the taps on its message act on.
type form struct {
	key                     pendingKey
	linkID, spaceID, action string
	typ, title              string
	version                 int64
	at                      time.Time
	options                 []string
	allowText               bool
	steps                   []string
	done                    []bool
	requiresAck             bool
	wiz                     *wizard
	head                    string // the text above the controls
}

// prompt is a free-text reply being waited for.
type prompt struct {
	linkID string
	form   pendingKey
	field  int // wizard field index, -1 for a question's answer
	at     time.Time
}

// Callback data (at most 64 bytes): "h<verb>:<space>:<action>[:<n>]", IDs without their prefix. Verbs: o open,
// q option n, t toggle step n, c complete, m acknowledge, f free-text answer, w wizard choice n, k skip the field,
// s submit with outcome n, x cancel. A bare "ho" on a notifier card resolves its action from the card mapping.
func humanData(verb byte, spaceID, actionID string, n int) string {
	d := fmt.Sprintf("h%c:%s:%s", verb, strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(actionID, "hum_"))
	if n >= 0 {
		d += ":" + strconv.Itoa(n)
	}
	return d
}

func isHumanData(d string) bool {
	return len(d) >= 2 && d[0] == 'h' && (d[1] == 'o' || strings.ContainsRune("qtcmfwksx", rune(d[1]))) && (len(d) == 2 || d[2] == ':')
}

// --- /needsyou ---

func (h *Handler) cmdNeedsYou(ctx context.Context, l updates.Link, arg string) {
	c, ok := h.caller(ctx, l)
	if !ok {
		return
	}
	s, msg := h.target(ctx, c, l, arg)
	if msg != "" {
		h.send(ctx, l.ChatID, reply{text: msg})
		return
	}
	h.setCurrent(l.ID, s.ID)
	var out struct {
		Items []humanAction `json:"items"`
	}
	r := c.get(ctx, "/spaces/"+s.ID+"/human-actions?status=open&limit="+strconv.Itoa(h.o.PageSize+1), &out)
	if !r.ok() {
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Needs you in %s", s.title())
	if len(out.Items) == 0 {
		b.WriteString("\nNothing is waiting for you.")
	}
	var rows [][]botapi.InlineKeyboardButton
	for i, a := range out.Items {
		if i == h.o.PageSize {
			b.WriteString("\n… more in the app")
			break
		}
		if !humanIDRe.MatchString(a.ID) {
			continue
		}
		rows = append(rows, []botapi.InlineKeyboardButton{button(typeMark(a.Type)+clean(a.Title, 50), humanData('o', s.ID, a.ID, -1))})
	}
	h.send(ctx, l.ChatID, reply{text: b.String(), markup: markup(rows...)})
}

func typeMark(t string) string {
	switch t {
	case "question":
		return "❓ "
	case "approval":
		return "🔐 "
	case "todo":
		return "☑ "
	case "message":
		return "✉ "
	}
	return "🧩 "
}

// --- state ---

func (h *Handler) addForm(f *form) {
	now := h.o.Clock.Now()
	f.at = now
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest pendingKey
	var oldestAt time.Time
	n := 0
	for k, x := range h.forms {
		switch {
		case now.Sub(x.at) > pendingTTL:
			delete(h.forms, k)
		case x.linkID == f.linkID:
			n++
			if oldestAt.IsZero() || x.at.Before(oldestAt) {
				oldest, oldestAt = k, x.at
			}
		}
	}
	if n >= maxFormsPerLink {
		delete(h.forms, oldest)
	}
	h.forms[f.key] = f
}

func (h *Handler) addPrompt(k pendingKey, p prompt) {
	now := h.o.Clock.Now()
	p.at = now
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest pendingKey
	var oldestAt time.Time
	n := 0
	for pk, x := range h.prompts {
		switch {
		case now.Sub(x.at) > pendingTTL:
			delete(h.prompts, pk)
		case x.linkID == p.linkID:
			n++
			if oldestAt.IsZero() || x.at.Before(oldestAt) {
				oldest, oldestAt = pk, x.at
			}
		}
	}
	if n >= maxPendingPerLink {
		delete(h.prompts, oldest)
	}
	h.prompts[k] = p
}

// formFor returns the live form of a message for a link, touching its clock.
func (h *Handler) formFor(k pendingKey, linkID string) *form {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.forms[k]
	if f == nil || f.linkID != linkID || h.o.Clock.Now().Sub(f.at) > pendingTTL {
		return nil
	}
	f.at = h.o.Clock.Now()
	return f
}

func (h *Handler) dropForm(k pendingKey) {
	h.mu.Lock()
	delete(h.forms, k)
	h.mu.Unlock()
}

// --- opening ---

func (h *Handler) humanCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, data string) {
	f := strings.Split(data, ":")
	verb := f[0][1]
	var spaceID, actionID string
	if len(f) == 1 && verb == 'o' {
		// A notifier card: the action comes from the card mapping, which must belong to this link.
		if h.o.Resolve == nil {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		sp, ac, link, ok := h.o.Resolve(ctx, q.Message.Chat.ID, q.Message.MessageID)
		if !ok || link != l.ID {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		spaceID, actionID = sp, ac
	} else {
		if want := map[bool]int{true: 4, false: 3}[strings.ContainsRune("qtwsk", rune(verb))]; len(f) != want {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		spaceID, actionID = "spc_"+f[1], "hum_"+f[2]
	}
	if !spaceIDRe.MatchString(spaceID) || !humanIDRe.MatchString(actionID) {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	s, r, ok := h.space(ctx, c, spaceID)
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	if verb == 'o' {
		h.openAction(ctx, q, l, c, s, actionID)
		return
	}
	n := -1
	if len(f) == 4 {
		v, err := strconv.Atoi(f[3])
		if err != nil || v < 0 || v > 1000 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		n = v
	}
	fm := h.formFor(pendingKey{q.Message.Chat.ID, q.Message.MessageID}, l.ID)
	if fm == nil || fm.spaceID != spaceID || fm.action != actionID {
		// Never respond from a form that isn't known: show the current state again.
		h.answer(ctx, q.ID, TextFormExpired)
		return
	}
	h.formAction(ctx, q, l, c, s, fm, verb, n)
}

// openAction reads an action and shows its form as a new message.
func (h *Handler) openAction(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, actionID string) {
	var a humanAction
	r := c.get(ctx, "/spaces/"+s.ID+"/human-actions/"+actionID, &a)
	if !r.ok() || a.ID != actionID || a.SpaceID != s.ID {
		if r.ok() {
			r.status = http.StatusNotFound
		}
		h.answer(ctx, q.ID, failText(r))
		return
	}
	h.answer(ctx, q.ID, "")
	fm := h.newForm(a, l, s)
	rp := h.renderForm(fm, a, s)
	m, ok := h.send(ctx, l.ChatID, rp)
	if ok && fm.interactive() {
		fm.key = pendingKey{l.ChatID, m.MessageID}
		h.addForm(fm)
		h.wizardPrompt(ctx, fm)
	}
}

func (f *form) interactive() bool {
	return f.typ == "question" || f.typ == "todo" || f.typ == "message" || f.wiz != nil
}

func (h *Handler) newForm(a humanAction, l updates.Link, s spaceRef) *form {
	f := &form{linkID: l.ID, spaceID: s.ID, action: a.ID, typ: a.Type, title: a.Title, version: a.Version}
	if !a.pending() {
		return f
	}
	switch a.Type {
	case "question":
		var q struct {
			Options   []string `json:"options"`
			AllowText bool     `json:"allow_text"`
		}
		_ = json.Unmarshal(a.Request, &q)
		if len(q.Options) > maxOptions {
			q.Options = q.Options[:maxOptions]
		}
		f.options, f.allowText = q.Options, q.AllowText || len(q.Options) == 0
	case "todo":
		var t struct {
			Steps []string `json:"steps"`
		}
		_ = json.Unmarshal(a.Request, &t)
		f.steps, f.done = t.Steps, make([]bool, len(t.Steps))
	case "message":
		var m struct {
			RequiresAck bool `json:"requires_ack"`
		}
		_ = json.Unmarshal(a.Request, &m)
		f.requiresAck = m.RequiresAck
	case "custom":
		f.wiz = parseWizard(a)
	}
	return f
}

// parseWizard returns a wizard for a custom action whose response schema is simple: a flat object of strings (with or
// without enum), numbers, integers and booleans. Anything else is nil (the Mini App form).
func parseWizard(a humanAction) *wizard {
	if a.TypeSpec == nil || len(a.TypeSpec.Outcomes) == 0 || len(a.TypeSpec.Outcomes) > 4 {
		return nil
	}
	var sch struct {
		Type       string                     `json:"type"`
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if json.Unmarshal(a.TypeSpec.ResponseSchema, &sch) != nil || (sch.Type != "" && sch.Type != "object") || len(sch.Properties) > maxWizardFields {
		return nil
	}
	var top map[string]json.RawMessage
	_ = json.Unmarshal(a.TypeSpec.ResponseSchema, &top)
	for k := range top {
		switch k {
		case "type", "properties", "required", "additionalProperties", "title", "description", "$schema":
		default:
			return nil
		}
	}
	var ui struct {
		Order  []string `json:"order"`
		Fields map[string]struct {
			Label string `json:"label"`
		} `json:"fields"`
	}
	_ = json.Unmarshal(a.TypeSpec.UI, &ui)
	names := make([]string, 0, len(sch.Properties))
	for _, n := range ui.Order {
		if _, ok := sch.Properties[n]; ok && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	rest := make([]string, 0, len(sch.Properties))
	for n := range sch.Properties {
		if !slices.Contains(names, n) {
			rest = append(rest, n)
		}
	}
	slices.Sort(rest)
	names = append(names, rest...)
	w := &wizard{values: map[string]any{}, outcomes: a.TypeSpec.Outcomes}
	for _, n := range names {
		var p struct {
			Type string            `json:"type"`
			Enum []json.RawMessage `json:"enum"`
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(sch.Properties[n], &raw)
		for k := range raw {
			switch k {
			case "type", "enum", "title", "description", "maxLength", "minLength", "minimum", "maximum":
			default:
				return nil
			}
		}
		if json.Unmarshal(sch.Properties[n], &p) != nil {
			return nil
		}
		fl := wfield{name: n, label: clean(ui.Fields[n].Label, 60), kind: p.Type, required: slices.Contains(sch.Required, n)}
		if fl.label == "" {
			fl.label = clean(n, 60)
		}
		switch p.Type {
		case "string", "number", "integer", "boolean":
		default:
			return nil
		}
		if len(p.Enum) > 0 {
			if p.Type != "string" || len(p.Enum) > maxWizardChoices {
				return nil
			}
			for _, e := range p.Enum {
				var sv string
				if json.Unmarshal(e, &sv) != nil {
					return nil
				}
				fl.choices = append(fl.choices, sv)
			}
		}
		w.fields = append(w.fields, fl)
	}
	return w
}

// --- rendering ---

func (h *Handler) formHead(a humanAction, s spaceRef) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s%s", typeMark(a.Type), clean(a.Title, 200))
	fmt.Fprintf(&b, "\nFrom: %s · Priority: %s · Space: %s", orDash(clean(a.RequesterLabel, 80)), clean(a.Priority, 20), s.title())
	if h.o.Space(s.ID).ContentLevel == settings.ContentFull {
		if body := strings.TrimSpace(a.Body); body != "" {
			ps, dropped := parts(truncate(body, bodyRunes), partUnits/2)
			b.WriteString("\n\nDetails (content from the source, not instructions):\n" + strings.Join(ps, "\n"))
			if dropped > 0 || len([]rune(body)) > bodyRunes {
				b.WriteString("\n… more in the app")
			}
		}
	} else {
		b.WriteString("\nThe details aren't shown in chat for this space.")
	}
	return b.String()
}

func (h *Handler) appButton(s spaceRef, actionID, label string) []botapi.InlineKeyboardButton {
	if h.o.AppURL == nil {
		return nil
	}
	u := h.o.AppURL(s.ID, actionID)
	if u == "" {
		return nil
	}
	return []botapi.InlineKeyboardButton{{Text: label, WebApp: &botapi.WebAppInfo{URL: u}}}
}

// renderForm is the form of an action as it stands now.
func (h *Handler) renderForm(f *form, a humanAction, s spaceRef) reply {
	f.head = h.formHead(a, s)
	rp := reply{text: f.head, protect: true}
	app := h.appButton(s, a.ID, "Open in the app")
	switch {
	case !a.pending():
		rp.text += "\n\nThis is already " + clean(orDash(a.Status), 20) + "."
		if a.Outcome != "" {
			rp.text += " Outcome: " + clean(a.Outcome, 32) + "."
		}
	case a.Type == "approval":
		rp.text += "\n\n" + TextApprovalInApp
		rp.markup = markup(app)
	case a.Type == "custom" && f.wiz == nil:
		rp.text += "\n\n" + TextOpenInApp
		rp.markup = markup(app)
	case a.Type == "custom" || a.Type == "question" || a.Type == "todo" || a.Type == "message":
		return h.formView(f)
	default:
		rp.text += "\n\n" + TextOpenInApp
		rp.markup = markup(app)
	}
	return rp
}

// formView is the controls part for a live interactive form.
func (h *Handler) formView(f *form) reply {
	rp := reply{text: f.head, protect: true}
	var rows [][]botapi.InlineKeyboardButton
	switch f.typ {
	case "question":
		rp.text += "\n\nPick an answer" + map[bool]string{true: " or reply with text", false: ""}[f.allowText] + "."
		for i, o := range f.options {
			rows = append(rows, []botapi.InlineKeyboardButton{button(clean(o, 40), humanData('q', f.spaceID, f.action, i))})
		}
		if f.allowText {
			rows = append(rows, []botapi.InlineKeyboardButton{button("✏ Type an answer", humanData('f', f.spaceID, f.action, -1))})
		}
	case "todo":
		all := true
		for i, st := range f.steps {
			mark := "⬜ "
			if f.done[i] {
				mark = "✅ "
			} else {
				all = false
			}
			rows = append(rows, []botapi.InlineKeyboardButton{button(mark+clean(st, 50), humanData('t', f.spaceID, f.action, i))})
		}
		if all {
			rows = append(rows, []botapi.InlineKeyboardButton{button("Mark completed", humanData('c', f.spaceID, f.action, -1))})
		} else {
			rp.text += "\n\nCheck every step to complete it."
		}
	case "message":
		label := "Dismiss"
		if f.requiresAck {
			label = "Acknowledge"
			rp.text += "\n\nThis needs your acknowledgement."
		} else {
			rp.text += "\n\nFor your information."
		}
		rows = append(rows, []botapi.InlineKeyboardButton{button(label, humanData('m', f.spaceID, f.action, -1))})
	case "custom":
		return h.wizardView(f)
	}
	rp.markup = markup(rows...)
	return rp
}

func (h *Handler) wizardView(f *form) reply {
	w := f.wiz
	rp := reply{text: f.head, protect: true}
	var rows [][]botapi.InlineKeyboardButton
	if w.idx >= len(w.fields) {
		rp.text += "\n\nYour answers:"
		for _, fl := range w.fields {
			v, ok := w.values[fl.name]
			if !ok {
				v = "—"
			}
			rp.text += fmt.Sprintf("\n• %s: %s", fl.label, clean(fmt.Sprint(v), 100))
		}
		var row []botapi.InlineKeyboardButton
		for i, o := range w.outcomes {
			row = append(row, button("Submit: "+clean(o, 24), humanData('s', f.spaceID, f.action, i)))
		}
		rows = append(rows, row, []botapi.InlineKeyboardButton{button("Cancel", humanData('x', f.spaceID, f.action, -1))})
		rp.markup = markup(rows...)
		return rp
	}
	fl := w.fields[w.idx]
	opt := ""
	if !fl.required {
		opt = " (optional)"
	}
	rp.text += fmt.Sprintf("\n\n%s%s — step %d of %d", fl.label, opt, w.idx+1, len(w.fields))
	switch {
	case fl.kind == "boolean":
		rows = append(rows, []botapi.InlineKeyboardButton{button("Yes", humanData('w', f.spaceID, f.action, 1)), button("No", humanData('w', f.spaceID, f.action, 0))})
	case len(fl.choices) > 0:
		for i, ch := range fl.choices {
			rows = append(rows, []botapi.InlineKeyboardButton{button(clean(ch, 40), humanData('w', f.spaceID, f.action, i))})
		}
	default:
		rp.text += "\nReply to the prompt below with the value."
	}
	var nav []botapi.InlineKeyboardButton
	if !fl.required {
		nav = append(nav, button("Skip", humanData('k', f.spaceID, f.action, w.idx)))
	}
	nav = append(nav, button("Cancel", humanData('x', f.spaceID, f.action, -1)))
	rows = append(rows, nav)
	rp.markup = markup(rows...)
	return rp
}

// refresh shows a form's current view on its message; when that can't be edited, on a new message the form moves to.
func (h *Handler) refresh(ctx context.Context, f *form) {
	rp := h.formView(f)
	b := h.bot()
	if b == nil {
		return
	}
	ectx, cancel := context.WithTimeout(ctx, apiTimeout)
	_, err := b.EditMessageText(ectx, botapi.EditMessageTextParams{ChatID: f.key.chat, MessageID: f.key.msg, Text: rp.text,
		LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}, ReplyMarkup: rp.markup})
	cancel()
	if err == nil {
		h.wizardPrompt(ctx, f)
		return
	}
	if m, ok := h.send(ctx, f.key.chat, rp); ok {
		h.mu.Lock()
		delete(h.forms, f.key)
		f.key = pendingKey{f.key.chat, m.MessageID}
		h.forms[f.key] = f
		h.mu.Unlock()
		h.wizardPrompt(ctx, f)
	}
}

// wizardPrompt asks for the current field's value when it is free text (ForceReply, bound to this prompt message).
func (h *Handler) wizardPrompt(ctx context.Context, f *form) {
	if f.wiz == nil || f.wiz.idx >= len(f.wiz.fields) {
		return
	}
	fl := f.wiz.fields[f.wiz.idx]
	if fl.kind == "boolean" || len(fl.choices) > 0 {
		return
	}
	m, ok := h.send(ctx, f.key.chat, reply{text: "Reply with: " + fl.label, force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: truncate(fl.label, 60)}})
	if ok {
		h.addPrompt(pendingKey{f.key.chat, m.MessageID}, prompt{linkID: f.linkID, form: f.key, field: f.wiz.idx})
	}
}

// --- taps on a form ---

func (h *Handler) formAction(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, f *form, verb byte, n int) {
	switch {
	case verb == 'q' && f.typ == "question" && n >= 0 && n < len(f.options):
		h.respond(ctx, c, l, q, s, f, map[string]any{"outcome": "answered", "answer": f.options[n]})
	case verb == 'f' && f.typ == "question" && f.allowText:
		h.answer(ctx, q.ID, "")
		m, ok := h.send(ctx, l.ChatID, reply{text: "Reply to this message with your answer.", force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Answer"}})
		if ok {
			h.addPrompt(pendingKey{l.ChatID, m.MessageID}, prompt{linkID: l.ID, form: f.key, field: -1})
		}
	case verb == 't' && f.typ == "todo" && n >= 0 && n < len(f.steps):
		h.mu.Lock()
		f.done[n] = !f.done[n]
		h.mu.Unlock()
		h.answer(ctx, q.ID, "")
		h.refresh(ctx, f)
	case verb == 'c' && f.typ == "todo":
		if slices.Contains(f.done, false) {
			h.answer(ctx, q.ID, "Check every step first.")
			return
		}
		h.respond(ctx, c, l, q, s, f, map[string]any{"outcome": "completed"})
	case verb == 'm' && f.typ == "message":
		h.respond(ctx, c, l, q, s, f, map[string]any{"outcome": "acknowledged"})
	case f.wiz != nil && verb == 'w' && f.wiz.idx < len(f.wiz.fields):
		fl := f.wiz.fields[f.wiz.idx]
		switch {
		case fl.kind == "boolean" && (n == 0 || n == 1):
			f.wiz.values[fl.name] = n == 1
		case len(fl.choices) > 0 && n >= 0 && n < len(fl.choices):
			f.wiz.values[fl.name] = fl.choices[n]
		default:
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		f.wiz.idx++
		h.answer(ctx, q.ID, "")
		h.refresh(ctx, f)
	case f.wiz != nil && verb == 'k' && f.wiz.idx < len(f.wiz.fields) && n == f.wiz.idx && !f.wiz.fields[f.wiz.idx].required:
		f.wiz.idx++
		h.answer(ctx, q.ID, "")
		h.refresh(ctx, f)
	case f.wiz != nil && verb == 's' && f.wiz.idx >= len(f.wiz.fields) && n >= 0 && n < len(f.wiz.outcomes):
		h.respond(ctx, c, l, q, s, f, map[string]any{"outcome": f.wiz.outcomes[n], "data": f.wiz.values})
	case verb == 'x' && f.wiz != nil:
		h.dropForm(f.key)
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, reply{text: f.head + "\n\nCancelled. Nothing was sent."})
	default:
		h.answer(ctx, q.ID, TextUnavailable)
	}
}

// respond sends the response as the link's principal with the version the form showed. On success the form is closed;
// on failure it says why, and a stale form is dropped.
func (h *Handler) respond(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, f *form, body map[string]any) {
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/human-actions/"+f.action+"/respond", body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, f.version)})
	if !r.ok() {
		msg := failText(r)
		if msg == TextStale {
			h.dropForm(f.key)
			msg = "That request changed or was already answered. Reopen it from /needsyou."
		}
		if q != nil {
			h.answer(ctx, q.ID, msg)
		} else {
			h.send(ctx, l.ChatID, reply{text: msg})
		}
		return
	}
	h.dropForm(f.key)
	if q != nil {
		h.answer(ctx, q.ID, "Done")
	}
	outcome, _ := body["outcome"].(string)
	h.show(ctx, l.ChatID, &botapi.Message{MessageID: f.key.msg}, reply{text: f.head + "\n\n✅ " + strings.ToUpper(outcome[:1]) + outcome[1:] + "."})
}

// --- free-text replies ---

// promptReply takes a reply to a free-text prompt (a question's answer or a wizard field). It reports whether the
// reply was one of this package's prompts for this link.
func (h *Handler) promptReply(ctx context.Context, m *botapi.Message, l updates.Link) bool {
	k := pendingKey{m.Chat.ID, m.ReplyToMessage.MessageID}
	h.mu.Lock()
	p, ok := h.prompts[k]
	if ok && p.linkID == l.ID && h.o.Clock.Now().Sub(p.at) <= pendingTTL {
		delete(h.prompts, k)
	} else {
		ok = false
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	f := h.formFor(p.form, l.ID)
	if f == nil {
		h.send(ctx, l.ChatID, reply{text: TextFormExpired})
		return true
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		h.send(ctx, l.ChatID, reply{text: "Send the value as text."})
		h.addPrompt(k, p)
		return true
	}
	c, ok := h.caller(ctx, l)
	if !ok {
		return true
	}
	s, r, ok := h.space(ctx, c, f.spaceID)
	if !ok {
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return true
	}
	if p.field < 0 {
		if f.typ == "question" && f.allowText {
			h.respond(ctx, c, l, nil, s, f, map[string]any{"outcome": "answered", "answer": truncate(text, 50000)})
		}
		return true
	}
	if f.wiz == nil || p.field != f.wiz.idx || p.field >= len(f.wiz.fields) {
		h.send(ctx, l.ChatID, reply{text: TextFormExpired})
		return true
	}
	fl := f.wiz.fields[p.field]
	var v any = truncate(text, 2000)
	switch fl.kind {
	case "integer":
		i, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			h.retryPrompt(ctx, f, fl, "a whole number")
			return true
		}
		v = i
	case "number":
		x, err := strconv.ParseFloat(text, 64)
		if err != nil {
			h.retryPrompt(ctx, f, fl, "a number")
			return true
		}
		v = x
	}
	f.wiz.values[fl.name] = v
	f.wiz.idx++
	h.refresh(ctx, f)
	return true
}

func (h *Handler) retryPrompt(ctx context.Context, f *form, fl wfield, what string) {
	m, ok := h.send(ctx, f.key.chat, reply{text: fl.label + " needs " + what + ". Reply with it.", force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: truncate(fl.label, 60)}})
	if ok {
		h.addPrompt(pendingKey{f.key.chat, m.MessageID}, prompt{linkID: f.linkID, form: f.key, field: f.wiz.idx})
	}
}
