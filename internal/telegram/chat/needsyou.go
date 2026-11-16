package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/card"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Needs-you forms (ADR-0083, plan §2.4, TG-7-1): the human actions waiting for a person, answered from the chat.
// Questions (options, free text), todos (step toggles, complete), messages (acknowledge) and custom types with a simple
// response schema (a wizard) are handled here, and so are approvals (TG-7-2, ADR-0083 decision 14):
//
//   - Approve shows a Confirm step first ("Approve: <action>? [Confirm approve] [Back]"); Deny asks for a note with a
//     ForceReply and asks again for an empty one. Both act on the form's version (If-Match) within its TTL.
//   - An enforced approval (ADR-0024: get-action-approval returns a record) shows its operation, resource, space,
//     expiry, uses and argument digest, never its arguments or the body that carries them (SM-APPR-005). It is decided
//     in the browser unless the space set enforced_approvals = confirm; then the first step (Approve, or Deny) arms a
//     confirmation on the server (Options.ArmApproval) and the second step (Confirm, or the note) passes it. The
//     server enforces all of this itself (api telegram_approvals.go): the chat's view of the setting is only display.
//
// Like everything in this package, a tap acts as the link's principal through the in-process API and responds with the
// version the person saw (If-Match). A form is bound to (chat, form message, link); a free-text reply is bound to
// (chat, prompt message, link) with a TTL. Taps whose form is unknown or expired never respond: the form is shown again
// so the person sees the current state first.

var humanIDRe = regexp.MustCompile(`^hum_[0-9a-z]{26}$`)

// Texts of the needs-you forms.
const (
	TextFormExpired   = "That form expired. Open it again from /needsyou."
	TextApprovalInApp = "This approval is enforced: it is decided in the browser. This space doesn't allow deciding it in chat."
	TextDenyNote      = "Why are you denying this? Reply to this message; a note is required."
	TextDenyNoteEmpty = "A denial needs a note. Nothing was sent; reply with why you deny it."
	TextOpenInApp     = "Open it in the app to answer."
	maxWizardFields   = 8
	maxWizardChoices  = 8
	maxFormsPerLink   = 10
	bodyRunes         = 1500
	maxOptions        = 20 // a question with more options is answered in the app
	maxSteps          = 20 // a todo with more steps is answered in the app
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
	DedupKey       string          `json:"dedup_key"`
	Assignee       string          `json:"assignee"`
	Attachments    []attachmentRef `json:"attachments"`
	Comments       []humanComment  `json:"comments"`
	ReminderCount  int64           `json:"reminder_count"`
	EscalatedAt    *time.Time      `json:"escalated_at"`
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
	maxLen      *int
	minLen      *int
	min, max    *float64
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
	big                     bool   // too many steps or options for a keyboard
	head                    string // the text above the controls
	// Approvals: the action to approve, whether an enforced approval stands behind it, the step a Confirm would finish
	// ("approved" after Approve), and the server's armed confirmation (enforced only) with the outcome it was armed for.
	apAction             string
	enforced, inChat     bool
	group                bool // opened in a bound group: an enforced approval is never decided there
	confirming           string
	armToken, armOutcome string
	// Common actions (needsyou_actions.go): who the card is claimed by and for whom it is shown, whether it is still
	// pending, how many attachments it has, and the enforced approval behind it (for redrawing its header).
	userID, assignee string
	pending          bool
	attN             int
	ap               *enforcedApproval
	// A form on a notifier card (cards.go): it lives on the card's own message, in that message's thread; back says
	// where its Back goes (backCard or backForm). headEnt formats head (the card header's bold title).
	fromCard bool
	thread   int
	back     int
	headEnt  []botapi.MessageEntity
}

// prompt is a free-text reply being waited for.
type prompt struct {
	linkID string
	form   pendingKey
	field  int // wizard field index, -1 for a question's answer, promptDenyNote for an approval's denial note
	at     time.Time
	tries  int // retries after an unusable comment reply (capped at maxPromptRetries)
}

// maxPromptRetries caps how often a comment or clarification prompt is asked again after an unusable reply.
const maxPromptRetries = 3

// promptDenyNote is prompt.field for an approval's denial note.
const promptDenyNote = -2

// Callback data (at most 64 bytes): "h<verb>:<space>:<action>[:<n>]", IDs without their prefix. Verbs: o open,
// q option n, t toggle step n, c complete, m acknowledge, f free-text answer, w wizard choice n, k skip the field,
// s submit with outcome n, x cancel, a approve (asks to confirm), y confirm approve, b back, d deny (asks for a note),
// l claim, u release, r comment, i ask the requester (clarify), p list attachments, e withdraw (asks to confirm on a new
// message), v confirm the withdrawal (carries the card's version: "hv:<space>:<action>:<version>"), g keep it,
// n the whole form on the card's message (More…), z back from that form to the card.
// A bare "ho" on a notifier card resolves its action from the card mapping. The builder is the card package's, shared
// with the notifier's cards.
func humanData(verb byte, spaceID, actionID string, n int) string { return card.Data(verb, spaceID, actionID, n) }

func isHumanData(d string) bool {
	return len(d) >= 2 && d[0] == 'h' && (d[1] == 'o' || strings.ContainsRune("qtcmfwksxaybdluripevgnz", rune(d[1]))) && (len(d) == 2 || d[2] == ':')
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
	h.setCurrent(ctx, l.ID, s.ID)
	// One call with pending=true: open and in_progress (assigned or escalated) actions in the space's rank order.
	var out struct {
		Items []humanAction `json:"items"`
	}
	if r := c.get(ctx, "/spaces/"+s.ID+"/human-actions?pending=true&limit="+strconv.Itoa(h.o.PageSize+1), &out); !r.ok() {
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
		// In a bound group the card is the group's: the tapper acts as their own link, and Handler.space admits only
		// the bound space, so the link the card was sent to doesn't matter (it is never the tapper's authority).
		if !ok || l.Group == nil && link != l.ID {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		spaceID, actionID = sp, ac
	} else {
		if want := map[bool]int{true: 4, false: 3}[strings.ContainsRune("qtwskv", rune(verb))]; len(f) != want {
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
	if verb == 'v' || verb == 'g' {
		// The buttons of a withdrawal's confirmation act on that message only, not on a card.
		var ver int64
		if verb == 'v' {
			v, err := strconv.ParseInt(f[3], 10, 64)
			if err != nil || v <= 0 {
				h.answer(ctx, q.ID, TextUnavailable)
				return
			}
			ver = v
		}
		h.cancelConfirmTap(ctx, q, l, c, s, actionID, verb, ver)
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
	h.formMu.Lock() // one change to a form at a time: taps and replies may be handled concurrently
	defer h.formMu.Unlock()
	key := pendingKey{q.Message.Chat.ID, q.Message.MessageID}
	fm := h.formFor(key, l.ID)
	if fm != nil && h.cardMoved(ctx, fm) {
		h.dropForm(key) // the notifier drew a newer state of the card over the form: the form is stale
		fm = nil
	}
	if fm == nil && isCardVerb(verb) {
		// A card's own button: the card message becomes the form (cards.go), never a new message.
		h.cardTap(ctx, q, l, c, s, actionID, verb, n)
		return
	}
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
	var ap *enforcedApproval
	if a.Type == "approval" {
		// Every status: the body of an enforced approval holds its arguments, so it is never shown unless the approval is
		// positively known to be advisory (SM-APPR-005).
		ap = h.approvalOf(ctx, c, s, a, fm)
	}
	fm.ap = ap
	rp := h.renderForm(fm, a, s, ap)
	m, ok := h.send(ctx, l.ChatID, rp)
	if ok && a.pending() { // every pending form is kept: the common actions act on it, the type's own only when interactive
		fm.key = pendingKey{l.ChatID, m.MessageID}
		h.addForm(fm)
		h.wizardPrompt(ctx, fm)
	}
}

func (f *form) interactive() bool {
	if f.big {
		return false
	}
	if f.typ == "approval" {
		return f.apAction != "" && (!f.enforced || f.inChat) // apAction is set only while the approval is pending
	}
	return f.typ == "question" || f.typ == "todo" || f.typ == "message" || f.wiz != nil
}

func styled(b botapi.InlineKeyboardButton, style string) botapi.InlineKeyboardButton {
	b.Style = style
	return b
}

// enforcedApproval is the part of an enforced approval (get-action-approval) a card shows. Never its arguments.
type enforcedApproval struct {
	ID            string    `json:"id"`
	Operation     string    `json:"operation"`
	Resource      string    `json:"resource"`
	ArgsDigest    string    `json:"args_digest"`
	UsesAllowed   int64     `json:"uses_allowed"`
	State         string    `json:"state"`
	HumanActionID string    `json:"human_action_id"`
	RequestExpiry time.Time `json:"request_expires_at"`
}

// approvalOf reads the enforced approval behind an approval action and sets the form's mode. Not found means advisory,
// unless the action carries the approvals package's dedup key ("approval:…"); any other answer counts as enforced with
// nothing to show (decided in the browser), never as advisory.
func (h *Handler) approvalOf(ctx context.Context, c caller, s spaceRef, a humanAction, f *form) *enforcedApproval {
	var ap enforcedApproval
	r := c.get(ctx, "/spaces/"+s.ID+"/human-actions/"+a.ID+"/approval", &ap)
	switch {
	case r.status == http.StatusNotFound && strings.HasPrefix(a.DedupKey, "approval:"):
		// The approvals package's dedup key without its row yet (or a failed link): enforced, as the server counts it.
		f.enforced = true
		return nil
	case r.status == http.StatusNotFound:
		return nil
	case !r.ok() || ap.HumanActionID != a.ID:
		f.enforced = true
		return nil
	}
	f.enforced = true
	f.inChat = !s.group && h.o.Space(s.ID).EnforcedApprovals == settings.ApprovalsConfirm
	return &ap
}

func (h *Handler) newForm(a humanAction, l updates.Link, s spaceRef) *form {
	f := &form{linkID: l.ID, spaceID: s.ID, action: a.ID, typ: a.Type, title: a.Title, version: a.Version,
		userID: l.UserID, assignee: a.Assignee, pending: a.pending(), attN: len(attachmentsOf(a)), group: l.Group != nil || s.group}
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
		f.big = len(q.Options) > maxOptions // answered in the app, like a large todo
		f.options, f.allowText = q.Options, q.AllowText || len(q.Options) == 0
	case "todo":
		var t struct {
			Steps []string `json:"steps"`
		}
		_ = json.Unmarshal(a.Request, &t)
		f.big = len(t.Steps) > maxSteps
		f.steps, f.done = t.Steps, make([]bool, len(t.Steps))
	case "message":
		var m struct {
			RequiresAck bool `json:"requires_ack"`
		}
		_ = json.Unmarshal(a.Request, &m)
		f.requiresAck = m.RequiresAck
	case "custom":
		f.wiz = parseWizard(a)
	case "approval":
		var ar struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(a.Request, &ar)
		f.apAction = clean(ar.Action, 200)
		if f.apAction == "" {
			f.apAction = clean(a.Title, 200)
		}
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
	w := &wizard{values: map[string]any{}}
	for _, o := range a.TypeSpec.Outcomes {
		if o == "" || len([]rune(o)) > 32 {
			return nil
		}
		w.outcomes = append(w.outcomes, o)
	}
	for _, n := range names {
		var p struct {
			Type      string            `json:"type"`
			Enum      []json.RawMessage `json:"enum"`
			MaxLength *int              `json:"maxLength"`
			MinLength *int              `json:"minLength"`
			Minimum   *float64          `json:"minimum"`
			Maximum   *float64          `json:"maximum"`
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
		fl := wfield{name: n, label: clean(ui.Fields[n].Label, 60), kind: p.Type, required: slices.Contains(sch.Required, n),
			maxLen: p.MaxLength, minLen: p.MinLength, min: p.Minimum, max: p.Maximum}
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

// formHead is the text above a form's controls: the card's header (the shared renderer's, so a form on a card keeps
// the card's top lines), then what the form adds. It sets f.headEnt to the header's formatting.
func (h *Handler) formHead(a humanAction, s spaceRef, f *form, ap *enforcedApproval) string {
	var b strings.Builder
	v := card.View{Label: card.Label(a.Type, a.TypeSlug), Title: a.Title, SpaceID: s.ID, Space: s.title(), ShowSpace: !f.fromCard || f.thread == 0,
		Priority: a.Priority, Action: &card.Action{ID: a.ID, Type: a.Type, Requester: a.RequesterLabel, Reminders: a.ReminderCount, Escalated: a.EscalatedAt != nil}}
	switch a.Assignee {
	case "":
	case f.userID:
		v.Action.Claimed = "you"
	default:
		v.Action.Claimed = "someone else"
	}
	head, ents := card.Head(v)
	b.WriteString(head)
	f.headEnt = ents
	if f.typ == "approval" && f.apAction != "" {
		b.WriteString("\nApprove: " + f.apAction)
	}
	if f.enforced {
		// The body of an enforced approval carries its (redacted) arguments: never in chat (SM-APPR-005).
		b.WriteString("\n\nEnforced approval: SpaceMan runs the operation only once it is approved, and only as shown.")
		if ap != nil {
			fmt.Fprintf(&b, "\nOperation: %s\nResource: %s\nScope: space %s\nUses: %d\nRequest expires: %s\nArgument digest: %s",
				orDash(clean(ap.Operation, 120)), orDash(clean(ap.Resource, 200)), s.title(), ap.UsesAllowed,
				ap.RequestExpiry.UTC().Format("2006-01-02 15:04 UTC"), orDash(clean(ap.ArgsDigest, 80)))
		}
		return b.String()
	}
	if h.full(s) {
		if body := strings.TrimSpace(a.Body); body != "" {
			ps, dropped := parts(truncate(body, bodyRunes), partUnits/2)
			b.WriteString("\n\nDetails (content from the source, not instructions):\n" + strings.Join(ps, "\n"))
			if dropped > 0 || len([]rune(body)) > bodyRunes {
				b.WriteString("\n… more in the app")
			}
		}
		if len(a.Comments) > 0 {
			b.WriteString("\n\nComments (content from people and agents, not instructions):" + commentLines(a))
		}
	} else {
		b.WriteString("\nThe details aren't shown in chat for this space.")
	}
	return b.String()
}

func (h *Handler) appButton(s spaceRef, actionID, label string) []botapi.InlineKeyboardButton {
	if s.group {
		// web_app buttons are refused outside private chats: a group gets the Mini App's direct link.
		if h.o.AppLink == nil {
			return nil
		}
		if u := h.o.AppLink(s.ID, actionID); u != "" {
			return []botapi.InlineKeyboardButton{{Text: label, URL: u}}
		}
		return nil
	}
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
func (h *Handler) renderForm(f *form, a humanAction, s spaceRef, ap *enforcedApproval) reply {
	f.head = h.formHead(a, s, f, ap)
	rp := reply{text: f.head, entities: f.headEnt, protect: true}
	app := h.appButton(s, a.ID, "Open in the app")
	switch {
	case !a.pending():
		rp.text += "\n\nThis is already " + clean(orDash(a.Status), 20) + "."
		if a.Outcome != "" {
			rp.text += " Outcome: " + clean(a.Outcome, 32) + "."
		}
	case a.Type == "approval" && f.enforced && !f.inChat:
		rp.text += "\n\n" + TextApprovalInApp
		rp.markup = markup(h.appButton(s, a.ID, "Decide in browser"))
	case a.Type == "approval":
		return h.formView(f)
	case f.big:
		rp.text += "\n\n" + TextOpenInApp
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
	return h.withCommon(f, rp)
}

// formView is the controls part for a live interactive form. A form on a card has a Back to the card under it (not
// during a confirmation step, which has its own Back).
func (h *Handler) formView(f *form) reply {
	rp := h.withCommon(f, h.formViewBase(f))
	if f.fromCard && f.confirming == "" {
		var rows [][]botapi.InlineKeyboardButton
		if rp.markup != nil {
			rows = rp.markup.InlineKeyboard
		}
		rp.markup = markup(append(rows, []botapi.InlineKeyboardButton{button("‹ Back", humanData(card.VerbCard, f.spaceID, f.action, -1))})...)
	}
	return rp
}

func (h *Handler) formViewBase(f *form) reply {
	rp := reply{text: f.head, entities: f.headEnt, protect: true}
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
	case "approval":
		if f.confirming == "approved" {
			rp.text += "\n\nApprove: " + orDash(f.apAction) + "?"
			if f.enforced {
				rp.text += "\nThis is the second step: confirming lets SpaceMan run the operation as shown."
			}
			rows = append(rows, []botapi.InlineKeyboardButton{
				styled(button("✅ Confirm approve", humanData('y', f.spaceID, f.action, -1)), botapi.StyleSuccess),
				button("Back", humanData('b', f.spaceID, f.action, -1))})
			break
		}
		rp.text += "\n\nApprove or deny. A denial needs a note."
		rows = append(rows, []botapi.InlineKeyboardButton{
			styled(button("✅ Approve", humanData('a', f.spaceID, f.action, -1)), botapi.StyleSuccess),
			styled(button("❌ Deny", humanData('d', f.spaceID, f.action, -1)), botapi.StyleDanger)})
	}
	rp.markup = markup(rows...)
	return rp
}

func (h *Handler) wizardView(f *form) reply {
	w := f.wiz
	rp := reply{text: f.head, entities: f.headEnt, protect: true}
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
	if h.draw(ctx, f) {
		h.wizardPrompt(ctx, f)
	}
}

// draw puts a form's current view on its message, or on a new message the form moves to when that can't be edited. It
// reports whether the view was shown.
func (h *Handler) draw(ctx context.Context, f *form) bool {
	rp := h.formView(f)
	if f.fromCard {
		// On a card: through the card lock, and never moved to a new message. A card that moved on drops the form.
		if !h.editCard(ctx, f.key, f.action, f.version, true, false, rp) {
			h.dropForm(f.key)
			return false
		}
		return true
	}
	b := h.bot()
	if b == nil {
		return false
	}
	ectx, cancel := context.WithTimeout(ctx, apiTimeout)
	_, err := b.EditMessageText(ectx, botapi.EditMessageTextParams{ChatID: f.key.chat, MessageID: f.key.msg, Text: rp.text, Entities: rp.entities,
		LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}, ReplyMarkup: rp.markup})
	cancel()
	if err == nil {
		return true
	}
	if m, ok := h.send(ctx, f.key.chat, rp); ok {
		h.mu.Lock()
		delete(h.forms, f.key)
		f.key = pendingKey{f.key.chat, m.MessageID}
		h.forms[f.key] = f
		h.mu.Unlock()
		return true
	}
	return false
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
	text := "Reply with: " + fl.label
	if fl.maxLen != nil && *fl.maxLen > maxWizardText {
		text += fmt.Sprintf("\n(Chat takes at most %d characters here; open the app for a longer value.)", maxWizardText)
	}
	m, ok := h.send(ctx, f.key.chat, reply{text: text, force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: truncate(fl.label, 60)}})
	if ok {
		h.addPrompt(pendingKey{f.key.chat, m.MessageID}, prompt{linkID: f.linkID, form: f.key, field: f.wiz.idx})
	}
}

// --- taps on a form ---

func (h *Handler) formAction(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, s spaceRef, f *form, verb byte, n int) {
	if !f.pending {
		h.answer(ctx, q.ID, TextFormExpired)
		return
	}
	if h.commonAction(ctx, q, l, c, s, f, verb) {
		return
	}
	if f.fromCard && (verb == card.VerbCard || verb == 'b' && f.back == backCard || verb == 'x' && f.wiz != nil) {
		// Back to the card: from the form (More…), from a step a quick action opened, or a wizard given up.
		h.answer(ctx, q.ID, "")
		h.backToCard(ctx, l, c, s, f)
		return
	}
	if !f.interactive() { // answered in the app: only the common actions are offered here
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
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
	case verb == 'a' && f.typ == "approval":
		if !h.arm(f, "approved") {
			h.answer(ctx, q.ID, TextFailed)
			return
		}
		f.confirming = "approved"
		h.answer(ctx, q.ID, "")
		h.refresh(ctx, f)
	case verb == 'b' && f.typ == "approval" && f.confirming != "":
		f.confirming, f.armToken, f.armOutcome = "", "", ""
		h.answer(ctx, q.ID, "")
		h.refresh(ctx, f)
	case verb == 'y' && f.typ == "approval" && f.confirming == "approved":
		f.confirming = "" // the server's confirmation is single use: a failed Confirm starts again from Approve
		if !h.respond(h.confirmed(ctx, f, "approved"), c, l, q, s, f, map[string]any{"outcome": "approved"}) && h.formFor(f.key, l.ID) != nil {
			h.refresh(ctx, f)
		}
	case verb == 'd' && f.typ == "approval":
		if !h.arm(f, "denied") {
			h.answer(ctx, q.ID, TextFailed)
			return
		}
		f.confirming = ""
		h.answer(ctx, q.ID, "")
		m, ok := h.send(ctx, l.ChatID, reply{text: TextDenyNote, force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Why deny?"}})
		if ok {
			h.addPrompt(pendingKey{l.ChatID, m.MessageID}, prompt{linkID: l.ID, form: f.key, field: promptDenyNote})
		}
	case verb == 'x' && f.wiz != nil:
		h.dropForm(f.key)
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, reply{text: f.head + "\n\nCancelled. Nothing was sent."})
	default:
		h.answer(ctx, q.ID, TextUnavailable)
	}
}

// respond sends the response as the link's principal with the version the form showed. On success the form is closed;
// on failure it says why, and a stale form is dropped. It reports whether the response was accepted.
func (h *Handler) respond(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, f *form, body map[string]any) bool {
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/human-actions/"+f.action+"/respond", body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, f.version)})
	if !r.ok() {
		msg := failText(r)
		if msg == TextStale {
			h.dropForm(f.key)
			msg = "That request changed or was already answered. Reopen it from /needsyou."
			if f.fromCard {
				msg = TextCardChanged
			}
		}
		if q != nil {
			h.answer(ctx, q.ID, msg)
		} else {
			h.send(ctx, l.ChatID, reply{text: msg})
		}
		if f.fromCard && msg == TextCardChanged {
			h.backToCard(ctx, l, c, s, f) // the card shows the server's state again
		}
		return false
	}
	h.dropForm(f.key)
	if q != nil {
		h.answer(ctx, q.ID, "Done")
	}
	outcome, _ := body["outcome"].(string)
	if f.fromCard {
		h.settleCard(ctx, l, s, f, r.body, "resolved", outcome) // the card collapses to its final line
		return true
	}
	h.show(ctx, l.ChatID, &botapi.Message{MessageID: f.key.msg}, reply{text: f.head + "\n\n✅ Done: " + clean(outcome, 32) + "."})
	return true
}

// arm prepares the decision an approval form's first step starts: for an enforced approval it arms the server's
// confirmation for this link, action, outcome and version (false: it couldn't, nothing goes on). An advisory approval
// needs none.
func (h *Handler) arm(f *form, outcome string) bool {
	f.armToken, f.armOutcome = "", ""
	if !f.enforced {
		return true
	}
	if f.group || !f.inChat || h.o.ArmApproval == nil {
		return false
	}
	t := h.o.ArmApproval(f.linkID, f.spaceID, f.action, outcome, f.version)
	if t == "" {
		return false
	}
	f.armToken, f.armOutcome = t, outcome
	return true
}

// confirmed is ctx for the second step of deciding an approval: it carries the armed confirmation when the form armed
// one for this outcome. The server decides whether one is needed.
func (h *Handler) confirmed(ctx context.Context, f *form, outcome string) context.Context {
	if f.armToken == "" || f.armOutcome != outcome {
		return ctx
	}
	t := f.armToken
	f.armToken, f.armOutcome = "", "" // single use on the server too
	return api.WithApprovalConfirmation(ctx, t)
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
	if p.field == promptComment || p.field == promptClarify {
		h.commentReply(ctx, m, l, k, p) // takes formMu itself, only around the form lookups and the API call
		return true
	}
	h.formMu.Lock()
	defer h.formMu.Unlock()
	f := h.formFor(p.form, l.ID)
	if f == nil {
		h.send(ctx, l.ChatID, reply{text: TextFormExpired})
		return true
	}
	text := strings.TrimSpace(m.Text)
	if text == "" && p.field == promptDenyNote {
		h.denyNotePrompt(ctx, f)
		return true
	}
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
	if p.field == promptDenyNote {
		if f.typ != "approval" {
			h.send(ctx, l.ChatID, reply{text: TextFormExpired})
			return true
		}
		if n := len([]rune(text)); n > maxNoteRunes {
			h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("That note is too long (%d characters, at most %d). Nothing was sent.", n, maxNoteRunes)})
			h.denyNotePrompt(ctx, f)
			return true
		}
		h.respond(h.confirmed(ctx, f, "denied"), c, l, nil, s, f, map[string]any{"outcome": "denied", "note": text})
		return true
	}
	if p.field < 0 {
		if f.typ == "question" && f.allowText {
			if n := len([]rune(text)); n > maxAnswerRunes {
				h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("That answer is too long (%d characters, at most %d). Nothing was sent; reply again with a shorter one.", n, maxAnswerRunes)})
				h.addPrompt(k, p)
				return true
			}
			h.respond(ctx, c, l, nil, s, f, map[string]any{"outcome": "answered", "answer": text})
		}
		return true
	}
	if f.wiz == nil || p.field != f.wiz.idx || p.field >= len(f.wiz.fields) {
		h.send(ctx, l.ChatID, reply{text: TextFormExpired})
		return true
	}
	fl := f.wiz.fields[p.field]
	v, bad := fl.parse(text)
	if bad != "" {
		h.retryPrompt(ctx, f, fl, bad)
		return true
	}
	f.wiz.values[fl.name] = v
	f.wiz.idx++
	h.refresh(ctx, f)
	return true
}

// maxWizardText is the longest free-text value taken when the schema sets no maxLength.
const maxWizardText = 2000

// maxAnswerRunes is the longest free-text answer to a question.
const maxAnswerRunes = 50000

// maxNoteRunes is the longest note (human.Response.Note).
const maxNoteRunes = 20000

// denyNotePrompt asks again for a denial note, bound to the same form.
func (h *Handler) denyNotePrompt(ctx context.Context, f *form) {
	m, ok := h.send(ctx, f.key.chat, reply{text: TextDenyNoteEmpty, force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Why deny?"}})
	if ok {
		h.addPrompt(pendingKey{f.key.chat, m.MessageID}, prompt{linkID: f.linkID, form: f.key, field: promptDenyNote})
	}
}

// parse checks a typed value against the field's kind and limits. On failure it returns what the field needs.
func (fl wfield) parse(text string) (any, string) {
	switch fl.kind {
	case "integer", "number":
		var x float64
		var v any
		if fl.kind == "integer" {
			i, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, "a whole number"
			}
			x, v = float64(i), i
		} else {
			f, err := strconv.ParseFloat(text, 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, "a number"
			}
			x, v = f, f
		}
		if fl.min != nil && x < *fl.min {
			return nil, "a value of at least " + strconv.FormatFloat(*fl.min, 'g', -1, 64)
		}
		if fl.max != nil && x > *fl.max {
			return nil, "a value of at most " + strconv.FormatFloat(*fl.max, 'g', -1, 64)
		}
		return v, ""
	}
	n := len([]rune(text))
	limit := maxWizardText
	if fl.maxLen != nil && *fl.maxLen < limit {
		limit = *fl.maxLen
	}
	if n > limit {
		return nil, fmt.Sprintf("at most %d characters (you sent %d)", limit, n)
	}
	if fl.minLen != nil && n < *fl.minLen {
		return nil, fmt.Sprintf("at least %d characters", *fl.minLen)
	}
	return text, ""
}

func (h *Handler) retryPrompt(ctx context.Context, f *form, fl wfield, what string) {
	m, ok := h.send(ctx, f.key.chat, reply{text: fl.label + " needs " + what + ". Nothing was saved; reply with it again.", force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: truncate(fl.label, 60)}})
	if ok {
		h.addPrompt(pendingKey{f.key.chat, m.MessageID}, prompt{linkID: f.linkID, form: f.key, field: f.wiz.idx})
	}
}
