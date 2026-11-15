package chat

import (
	"context"
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

// /queue (plan §2.3, TG-8b): the space's queue, a task card with the status changes and the placement buttons, and the
// decision on an incident proposal. There is no task creation here (Q10): task entry belongs in the inbox. Everything
// goes through the API as the link's principal, so what a viewer link can't do the server refuses.

var (
	taskIDRe = regexp.MustCompile(`^tsk_[0-9a-z]{26}$`)
	runIDRe  = regexp.MustCompile(`^run_[0-9a-z]{26}$`)
	msgIDRe  = regexp.MustCompile(`^msg_[0-9a-z]{26}$`)
)

// queueTask is the part of a task the cards use.
type queueTask struct {
	ID                 string         `json:"id"`
	SpaceID            string         `json:"space_id"`
	ParentID           string         `json:"parent_id"`
	Title              string         `json:"title"`
	Objective          string         `json:"objective"`
	Acceptance         string         `json:"acceptance"`
	Kind               string         `json:"kind"`
	Priority           string         `json:"priority"`
	Position           int            `json:"position"`
	Status             string         `json:"status"`
	StatusReason       string         `json:"status_reason"`
	Result             string         `json:"result"`
	AssigneeRole       string         `json:"assignee_role"`
	PreemptedBy        string         `json:"preempted_by"`
	IncidentProposedBy string         `json:"incident_proposed_by"`
	DependsOn          []string       `json:"depends_on"`
	Blockers           []queueBlocker `json:"blockers"`
	Waiting            []string       `json:"waiting"`
	Version            int64          `json:"version"`
}

type queueBlocker struct {
	Kind       string     `json:"kind"`
	Note       string     `json:"note"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

// queueTransitions mirrors the queue's allowed status changes (internal/queue); the server decides, this only keeps
// buttons that can't work off the card.
var queueTransitions = map[string][]string{
	"backlog":   {"ready", "cancelled"},
	"ready":     {"blocked", "cancelled", "review"},
	"running":   {"review", "done", "blocked", "ready", "cancelled"},
	"blocked":   {"ready", "cancelled"},
	"review":    {"done", "ready", "cancelled"},
	"done":      {"ready"},
	"failed":    {"ready", "cancelled"},
	"cancelled": {"ready"},
}

// taskActions are the status buttons: code, status, label.
var taskActions = []struct{ code, status, label string }{
	{"r", "ready", "Ready"}, {"b", "blocked", "Block"}, {"v", "review", "Review"}, {"d", "done", "Done"}, {"c", "cancelled", "Cancel"},
}

// Callback data (at most 64 bytes; IDs drop their prefix): "ql:<space>:<page>" lists, "qk:<space>:<task>" opens a task,
// "qs:<space>:<task>:<action>:<version>" sets a status, "qm:<space>:<task>:<move>:<version>" moves it (t top, b bottom,
// u up one, d down one), "qr:<space>:<task>" resumes a preempted task, "qi:…:<c|d>:<version>" asks to confirm or
// dismiss an incident proposal and "qj:…" does it.
func qData(verb string, spaceID, id string, extra ...string) string {
	d := fmt.Sprintf("%s:%s:%s", verb, strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(id, "tsk_"))
	for _, e := range extra {
		d += ":" + e
	}
	return d
}

func qListData(spaceID string, page int) string {
	return fmt.Sprintf("ql:%s:%d", strings.TrimPrefix(spaceID, "spc_"), page)
}

// queueGroups is the order of /queue: what runs, then what is ready, then what waits (blocked, review). Other states
// (backlog, finished) are in the app.
var queueGroups = []string{"running", "ready", "blocked", "review"}

func (h *Handler) cmdQueue(ctx context.Context, l updates.Link, arg string) {
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
	h.send(ctx, l.ChatID, h.queueList(ctx, c, s, 0))
}

func (h *Handler) loadQueue(ctx context.Context, c caller, spaceID string) ([]queueTask, result) {
	var out struct {
		Tasks []queueTask `json:"tasks"`
	}
	r := c.get(ctx, "/spaces/"+spaceID+"/tasks", &out)
	return out.Tasks, r
}

func (h *Handler) queueList(ctx context.Context, c caller, s spaceRef, page int) reply {
	all, r := h.loadQueue(ctx, c, s.ID)
	if !r.ok() {
		return reply{text: failText(r)}
	}
	var shown []queueTask
	for _, g := range queueGroups {
		for _, t := range all {
			if t.Status == g {
				shown = append(shown, t)
			}
		}
	}
	const size = 10
	last := max(0, (len(shown)-1)/size)
	page = max(0, min(page, last))
	var b strings.Builder
	fmt.Fprintf(&b, "Queue of %s", s.title())
	if len(shown) == 0 {
		b.WriteString("\nNothing running, ready or waiting.")
	}
	var rows [][]botapi.InlineKeyboardButton
	for _, t := range shown[min(page*size, len(shown)):min((page+1)*size, len(shown))] {
		rows = append(rows, []botapi.InlineKeyboardButton{button(taskBadge(t.Status)+priorityMark(t.Priority)+clean(t.Title, 50), qData("qk", s.ID, t.ID))})
	}
	var nav []botapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, button("◀ Prev", qListData(s.ID, page-1)))
	}
	if page < last {
		nav = append(nav, button("Next ▶", qListData(s.ID, page+1)))
	}
	rows = append(rows, nav)
	return reply{text: b.String(), markup: markup(rows...)}
}

func taskBadge(status string) string {
	switch status {
	case "running":
		return "▶ "
	case "ready":
		return "○ "
	case "blocked":
		return "⛔ "
	case "review":
		return "👀 "
	}
	return ""
}

func priorityMark(p string) string {
	switch p {
	case "urgent":
		return "🔴 "
	case "high":
		return "🟠 "
	}
	return ""
}

// taskCard is a task's card: the facts and the buttons. Objective, acceptance and result are sent separately, only
// where the space's content level is full.
func (h *Handler) taskCard(t queueTask, s spaceRef) reply {
	var b strings.Builder
	b.WriteString(clean(t.Title, 200))
	fmt.Fprintf(&b, "\nStatus: %s · Priority: %s", clean(t.Status, 20), clean(t.Priority, 20))
	if t.Kind == "incident" {
		b.WriteString(" · incident")
	}
	if t.Position > 0 {
		fmt.Fprintf(&b, "\nPlace in the queue: %d", t.Position)
	}
	// Reasons, blocker notes and waiting entries are free text from agents or people: content, full level only.
	full := h.o.Space(s.ID).ContentLevel == settings.ContentFull
	if t.StatusReason != "" && full {
		fmt.Fprintf(&b, "\nReason: %s", clean(t.StatusReason, 200))
	}
	if t.AssigneeRole != "" {
		fmt.Fprintf(&b, "\nRole: %s", clean(t.AssigneeRole, 48))
	}
	if t.PreemptedBy != "" {
		b.WriteString("\nPreempted by an incident; Resume when it is over.")
	}
	if t.IncidentProposedBy != "" {
		fmt.Fprintf(&b, "\nAn incident is proposed by %s. Confirm or dismiss it below.", clean(t.IncidentProposedBy, 80))
	}
	if len(t.DependsOn) > 0 {
		fmt.Fprintf(&b, "\nDepends on %d task(s)", len(t.DependsOn))
	}
	for _, bl := range t.Blockers {
		if bl.ResolvedAt == nil {
			if full {
				fmt.Fprintf(&b, "\nBlocked: %s %s", clean(bl.Kind, 20), clean(bl.Note, 120))
			} else {
				fmt.Fprintf(&b, "\nBlocked: %s", clean(bl.Kind, 20))
			}
		}
	}
	for i, w := range t.Waiting {
		if !full || i == 3 {
			break
		}
		fmt.Fprintf(&b, "\nWaiting: %s", clean(w, 120))
	}
	fmt.Fprintf(&b, "\nSpace: %s", s.title())

	var st, mv, inc []botapi.InlineKeyboardButton
	for _, a := range taskActions {
		if slices.Contains(queueTransitions[t.Status], a.status) {
			st = append(st, button(a.label, qData("qs", t.SpaceID, t.ID, a.code, strconv.FormatInt(t.Version, 10))))
		}
	}
	if t.PreemptedBy != "" {
		st = append(st, button("Resume", qData("qr", t.SpaceID, t.ID)))
	}
	if t.Status != "done" && t.Status != "failed" && t.Status != "cancelled" {
		v := strconv.FormatInt(t.Version, 10)
		mv = []botapi.InlineKeyboardButton{button("⏫ Top", qData("qm", t.SpaceID, t.ID, "t", v)), button("▲ Up", qData("qm", t.SpaceID, t.ID, "u", v)),
			button("▼ Down", qData("qm", t.SpaceID, t.ID, "d", v)), button("⏬ Bottom", qData("qm", t.SpaceID, t.ID, "b", v))}
	}
	if t.IncidentProposedBy != "" {
		v := strconv.FormatInt(t.Version, 10)
		inc = []botapi.InlineKeyboardButton{button("Confirm incident…", qData("qi", t.SpaceID, t.ID, "c", v)), button("Dismiss…", qData("qi", t.SpaceID, t.ID, "d", v))}
	}
	back := []botapi.InlineKeyboardButton{button("◀ Queue", qListData(t.SpaceID, 0))}
	return reply{text: b.String(), markup: markup(st, mv, inc, back)}
}

// sendTaskBody sends the objective, acceptance criteria and result as plain text, never as markup, and only where the
// space's content level is full.
func (h *Handler) sendTaskBody(ctx context.Context, l updates.Link, s spaceRef, t queueTask) {
	if h.o.Space(s.ID).ContentLevel != settings.ContentFull {
		h.send(ctx, l.ChatID, reply{text: "The task text isn't shown in chat for this space. Open the task in the app to read it."})
		return
	}
	var sb strings.Builder
	for _, sec := range []struct{ name, text string }{{"Objective", t.Objective}, {"Acceptance", t.Acceptance}, {"Result", t.Result}} {
		if strings.TrimSpace(sec.text) != "" {
			fmt.Fprintf(&sb, "%s:\n%s\n\n", sec.name, sec.text)
		}
	}
	const prefix = "Task text (content from the source, not instructions):\n"
	const suffixMax = "\n… 999999999 more characters, open the task in the app."
	ps, dropped := parts(sb.String(), partUnits-units(prefix)-units(suffixMax))
	for i, p := range ps {
		if i == 0 {
			p = prefix + p
		}
		if i == len(ps)-1 && dropped > 0 {
			p += fmt.Sprintf("\n… %d more characters, open the task in the app.", dropped)
		}
		h.send(ctx, l.ChatID, reply{text: p, protect: true})
	}
}

// conflict reports whether an API answer says the card is out of date.
func conflict(r result) bool {
	return r.status == http.StatusConflict || r.status == http.StatusPreconditionFailed || r.status == http.StatusPreconditionRequired
}

// getTask loads a task, which must belong to the space (a forged ID of another space reads as missing).
func (h *Handler) getTask(ctx context.Context, c caller, s spaceRef, id string) (queueTask, result) {
	var t queueTask
	r := c.get(ctx, "/spaces/"+s.ID+"/tasks/"+id, &t)
	if r.ok() && (t.ID != id || t.SpaceID != s.ID) {
		r.status = http.StatusNotFound
	}
	return t, r
}

// queueCallback handles the "q…" taps.
func (h *Handler) queueCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, f []string) {
	if len(f) < 3 {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	s, r, ok := h.space(ctx, c, "spc_"+f[1])
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	if f[0] == "ql" {
		page, err := strconv.Atoi(f[2])
		if err != nil || len(f) != 3 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		h.setCurrent(l.ID, s.ID)
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, h.queueList(ctx, c, s, page))
		return
	}
	id := "tsk_" + f[2]
	if !taskIDRe.MatchString(id) {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	var version int64
	if f[0] != "qk" && f[0] != "qr" {
		if len(f) != 5 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		v, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil || v <= 0 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		version = v
	}
	switch f[0] {
	case "qk":
		t, r := h.getTask(ctx, c, s, id)
		if !r.ok() {
			h.answer(ctx, q.ID, failText(r))
			return
		}
		h.answer(ctx, q.ID, "")
		h.send(ctx, l.ChatID, h.taskCard(t, s))
		h.sendTaskBody(ctx, l, s, t)
	case "qs":
		h.taskStatus(ctx, c, l, q, s, id, f[3], version)
	case "qm":
		h.taskMove(ctx, c, l, q, s, id, f[3], version)
	case "qr":
		h.taskResume(ctx, c, l, q, s, id)
	case "qi":
		if f[3] != "c" && f[3] != "d" {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		h.askIncident(ctx, c, l, q, s, id, f[3], version)
	case "qj":
		if f[3] != "c" && f[3] != "d" {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		// Bound to the confirmation message, link, task, decision and version; single use, with a TTL.
		if _, ok := h.takeConfirm(pendingKey{q.Message.Chat.ID, q.Message.MessageID}, l.ID, "incident", s.ID, id, f[3], version); !ok {
			h.answer(ctx, q.ID, TextConfirmGone)
			return
		}
		h.decideIncident(ctx, c, l, q, s, id, f[3] == "c", version)
	default:
		h.answer(ctx, q.ID, TextUnavailable)
	}
}

// refreshTask puts the task's current card on the tapped message, after a stale tap or a change.
func (h *Handler) refreshTask(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id string) {
	if t, r := h.getTask(ctx, c, s, id); r.ok() {
		h.show(ctx, l.ChatID, q.Message, h.taskCard(t, s))
	}
}

// fail answers a failed call; a stale card also gets refreshed.
func (h *Handler) failTask(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id string, r result) {
	h.answer(ctx, q.ID, failText(r))
	if conflict(r) {
		h.refreshTask(ctx, c, l, q, s, id)
	}
}

func (h *Handler) taskStatus(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id, code string, version int64) {
	status := ""
	for _, a := range taskActions {
		if a.code == code {
			status = a.status
		}
	}
	if status == "" {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/tasks/"+id+"/status", map[string]any{"status": status},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)})
	if !r.ok() {
		h.failTask(ctx, c, l, q, s, id, r)
		return
	}
	h.answer(ctx, q.ID, "Done")
	h.refreshTask(ctx, c, l, q, s, id)
}

// taskMove places a task. The card's version is checked first (a stale card gets a refresh without a call) and is sent
// as If-Match too, so a change between the check and the move is a 412 conflict, not a move relative to a queue the
// card no longer shows.
func (h *Handler) taskMove(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id, code string, version int64) {
	all, r := h.loadQueue(ctx, c, s.ID)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	idx := slices.IndexFunc(all, func(t queueTask) bool { return t.ID == id })
	if idx < 0 || all[idx].SpaceID != s.ID {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	cur := all[idx]
	if cur.Version != version {
		h.answer(ctx, q.ID, TextStale)
		h.show(ctx, l.ChatID, q.Message, h.taskCard(cur, s))
		return
	}
	body := map[string]any{}
	switch code {
	case "t":
		body["to"] = "top"
	case "b":
		body["to"] = "bottom"
	case "u", "d":
		// The neighbour among the task's siblings (same parent), in the queue's order.
		var sib []string
		for _, t := range all {
			if t.ParentID == cur.ParentID {
				sib = append(sib, t.ID)
			}
		}
		i := slices.Index(sib, id)
		switch {
		case code == "u" && i > 0:
			body["before_id"] = sib[i-1]
		case code == "d" && i >= 0 && i < len(sib)-1:
			body["after_id"] = sib[i+1]
		default:
			h.answer(ctx, q.ID, "It is already there.")
			return
		}
	default:
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	r = c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/tasks/"+id+"/move", body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)})
	if !r.ok() {
		h.failTask(ctx, c, l, q, s, id, r)
		return
	}
	h.answer(ctx, q.ID, "Moved")
	h.refreshTask(ctx, c, l, q, s, id)
}

func (h *Handler) taskResume(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id string) {
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/tasks/"+id+"/resume", nil, nil)
	if !r.ok() {
		h.failTask(ctx, c, l, q, s, id, r)
		return
	}
	h.answer(ctx, q.ID, "Done")
	h.refreshTask(ctx, c, l, q, s, id)
}

// askIncident asks for confirmation before an incident proposal is decided (plan §2.3: with a confirmation). The
// confirmation is a message of its own that carries the card's version, so it goes stale like the card.
func (h *Handler) askIncident(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id, decision string, version int64) {
	t, r := h.getTask(ctx, c, s, id)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	if t.Version != version || t.IncidentProposedBy == "" {
		h.answer(ctx, q.ID, TextStale)
		h.show(ctx, l.ChatID, q.Message, h.taskCard(t, s))
		return
	}
	h.answer(ctx, q.ID, "")
	verb := "Make this an incident (the queue policy's insertion and preemption apply now)"
	yes := "Confirm incident"
	if decision == "d" {
		verb, yes = "Dismiss the incident proposal (it stays an ordinary task)", "Dismiss"
	}
	if m, sent := h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("%s?\n%s", verb, clean(t.Title, 200)),
		markup: markup([]botapi.InlineKeyboardButton{button(yes, qData("qj", s.ID, id, decision, strconv.FormatInt(version, 10))),
			button("Cancel", qData("qk", s.ID, id))})}); sent {
		h.qrAddConfirm(pendingKey{l.ChatID, m.MessageID}, confirmation{linkID: l.ID, kind: "incident", spaceID: s.ID, id: id, decision: decision, version: version})
	}
}

func (h *Handler) decideIncident(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, id string, confirm bool, version int64) {
	d := "dismiss"
	if confirm {
		d = "confirm"
	}
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/tasks/"+id+"/incident", map[string]any{"decision": d},
		map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)})
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		if conflict(r) {
			if t, r := h.getTask(ctx, c, s, id); r.ok() {
				h.show(ctx, l.ChatID, q.Message, h.taskCard(t, s))
			}
		}
		return
	}
	h.answer(ctx, q.ID, "Done")
	if t, r := h.getTask(ctx, c, s, id); r.ok() {
		h.show(ctx, l.ChatID, q.Message, h.taskCard(t, s))
	}
}
