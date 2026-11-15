package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

type spaceRef struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	State string `json:"state"`

	PauseMode string `json:"pause_mode"`
}

var spaceIDRe = regexp.MustCompile(`^spc_[0-9a-z]{26}$`)

func (r spaceRef) title() string {
	if n := clean(r.Name, 60); n != "" {
		return n
	}
	return clean(r.Slug, 60)
}

// visibleSpaces lists the spaces the link's user may read (the API filters them) that Telegram may see (the space's
// settings). A space failing either test is simply absent.
func (h *Handler) visibleSpaces(ctx context.Context, c caller) ([]spaceRef, result) {
	var out struct {
		Items []spaceRef `json:"items"`
	}
	r := c.get(ctx, "/spaces", &out)
	if !r.ok() {
		return nil, r
	}
	var vis []spaceRef
	for _, s := range out.Items {
		if h.visible(s.ID) {
			vis = append(vis, s)
		}
	}
	return vis, r
}

// visible reports whether Telegram may see a space at all.
func (h *Handler) visible(spaceID string) bool {
	return h.o.Space != nil && spaceIDRe.MatchString(spaceID) && h.o.Space(spaceID).Visible()
}

// space loads one space for the user: hidden from Telegram, unreadable or missing all give ok=false.
func (h *Handler) space(ctx context.Context, c caller, id string) (spaceRef, result, bool) {
	if !h.visible(id) {
		return spaceRef{}, result{status: http.StatusNotFound}, false
	}
	var s spaceRef
	r := c.get(ctx, "/spaces/"+id, &s)
	return s, r, r.ok()
}

// resolve finds a space by ID, slug or name (exactly, ignoring case) among the visible ones.
func (h *Handler) resolve(ctx context.Context, c caller, ref string) (spaceRef, bool) {
	vis, r := h.visibleSpaces(ctx, c)
	if !r.ok() {
		return spaceRef{}, false
	}
	for _, s := range vis {
		if s.ID == ref {
			return s, true // an exact ID wins over a slug or name that happens to look like it
		}
	}
	for _, s := range vis {
		if strings.EqualFold(s.Slug, ref) || strings.EqualFold(s.Name, ref) {
			return s, true
		}
	}
	return spaceRef{}, false
}

// setCurrent makes spaceID the link's current space. Work done in a space's topic leaves it alone: the topic already
// says which space, and a note written there must not redirect what the user writes in the main chat.
func (h *Handler) setCurrent(ctx context.Context, linkID, spaceID string) {
	if pinnedOf(ctx) != "" {
		return
	}
	h.setCurrentNow(linkID, spaceID)
}

func (h *Handler) setCurrentNow(linkID, spaceID string) {
	h.mu.Lock()
	h.current[linkID] = spaceID
	h.mu.Unlock()
}

func (h *Handler) currentSpace(linkID string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current[linkID]
}

// target picks the space a command works on: the one named, else the one of the topic written in, else the chosen one, else the only visible one.
func (h *Handler) target(ctx context.Context, c caller, l updates.Link, ref string) (spaceRef, string) {
	if ref != "" {
		if s, ok := h.resolve(ctx, c, ref); ok {
			return s, ""
		}
		return spaceRef{}, TextUnavailable
	}
	if pin := pinnedOf(ctx); pin != "" {
		// Written in a space's topic: that space, authorized again like a named one. Never the current space instead.
		// By ID only: slugs and names are free text, so a space renamed to another's ID must not take its topic.
		if s, _, ok := h.space(ctx, c, pin); ok {
			return s, ""
		}
		return spaceRef{}, TextUnavailable
	}
	if id := h.currentSpace(l.ID); id != "" {
		if s, _, ok := h.space(ctx, c, id); ok {
			return s, ""
		}
	}
	vis, r := h.visibleSpaces(ctx, c)
	switch {
	case !r.ok():
		return spaceRef{}, failText(r)
	case len(vis) == 1:
		return vis[0], ""
	case len(vis) == 0:
		return spaceRef{}, TextNoSpaces
	}
	return spaceRef{}, TextPickSpace
}

func (h *Handler) cmdSpaces(ctx context.Context, l updates.Link) {
	c, ok := h.caller(ctx, l)
	if !ok {
		return
	}
	h.send(ctx, l.ChatID, h.spacesReply(ctx, c))
}

func (h *Handler) spacesReply(ctx context.Context, c caller) reply {
	vis, r := h.visibleSpaces(ctx, c)
	if !r.ok() {
		return reply{text: failText(r)}
	}
	if len(vis) == 0 {
		return reply{text: TextNoSpaces}
	}
	badges := h.badges(ctx, c)
	var rows [][]botapi.InlineKeyboardButton
	for _, s := range vis {
		label := s.title()
		if b := badges[s.ID]; b != "" {
			label += "  " + b
		}
		rows = append(rows, []botapi.InlineKeyboardButton{button(label, "sp:"+s.ID)})
	}
	return reply{text: "Your spaces. Tap one to switch to it and see its overview.", markup: markup(rows...)}
}

// badges reads the attention summary once and returns a short badge line per space: pending human actions, blocked
// or failed tasks and failed runs. The API leaves out what the user may not read.
func (h *Handler) badges(ctx context.Context, c caller) map[string]string {
	var att struct {
		Spaces []struct {
			SpaceID      string `json:"space_id"`
			HumanPending *int64 `json:"human_pending"`
			TasksBlocked *int64 `json:"tasks_blocked"`
			TasksFailed  *int64 `json:"tasks_failed"`
			RunsFailed   *int64 `json:"runs_failed"`
		} `json:"spaces"`
	}
	if r := c.get(ctx, "/attention", &att); !r.ok() {
		return nil
	}
	out := map[string]string{}
	for _, s := range att.Spaces {
		var b []string
		add := func(sym string, n *int64) {
			if n != nil && *n > 0 {
				b = append(b, fmt.Sprintf("%s%d", sym, *n))
			}
		}
		add("🙋", s.HumanPending)
		add("⛔", s.TasksBlocked)
		add("❌", s.TasksFailed)
		add("💥", s.RunsFailed)
		out[s.SpaceID] = strings.Join(b, " ")
	}
	return out
}

func (h *Handler) cmdSpace(ctx context.Context, l updates.Link, arg string) {
	c, ok := h.caller(ctx, l)
	if !ok {
		return
	}
	s, msg := h.target(ctx, c, l, arg)
	if msg != "" {
		h.send(ctx, l.ChatID, reply{text: msg})
		return
	}
	h.setCurrentNow(l.ID, s.ID) // an explicit /space moves it even from a topic
	h.send(ctx, l.ChatID, h.overview(ctx, c, s))
}

// caller builds the update's principal; without one (revoked link, disabled user) the user is told once and nothing runs.
func (h *Handler) caller(ctx context.Context, l updates.Link) (caller, bool) {
	p := h.principal(ctx, l)
	if p == nil {
		h.send(ctx, l.ChatID, reply{text: TextNoAccess})
		return caller{}, false
	}
	return caller{h: h, p: p}, true
}

// overview is the /space card: state, control run, running and next tasks, what needs a person, the inbox.
func (h *Handler) overview(ctx context.Context, c caller, s spaceRef) reply {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", s.title())
	if s.Slug != "" && s.Slug != s.Name {
		fmt.Fprintf(&b, " (%s)", clean(s.Slug, 40))
	}
	state := s.State
	if s.PauseMode != "" {
		state += ", " + s.PauseMode
	}
	fmt.Fprintf(&b, "\nState: %s", clean(state, 40))

	var runs struct {
		Items []struct {
			Kind     string `json:"kind"`
			Observed string `json:"observed"`
		} `json:"items"`
	}
	if r := c.get(ctx, "/spaces/"+s.ID+"/runs?limit=20", &runs); r.ok() {
		control := "none"
		for _, ru := range runs.Items {
			if ru.Kind == "control" {
				control = clean(ru.Observed, 20)
				break
			}
		}
		fmt.Fprintf(&b, "\nControl run: %s", control)
	}

	var q struct {
		Tasks []struct {
			Title  string `json:"title"`
			Status string `json:"status"`
		} `json:"tasks"`
	}
	if r := c.get(ctx, "/spaces/"+s.ID+"/tasks", &q); r.ok() {
		var running []string
		next := ""
		counts := map[string]int{}
		for _, t := range q.Tasks {
			counts[t.Status]++
			switch {
			case t.Status == "running" && len(running) < 3:
				running = append(running, clean(t.Title, 60))
			case t.Status == "ready" && next == "":
				next = clean(t.Title, 60)
			}
		}
		if counts["running"] > 0 {
			fmt.Fprintf(&b, "\nRunning tasks: %d (%s)", counts["running"], strings.Join(running, "; "))
		} else {
			b.WriteString("\nRunning tasks: none")
		}
		if next != "" {
			fmt.Fprintf(&b, "\nNext ready: %s", next)
		}
		fmt.Fprintf(&b, "\nBlocked: %d · In review: %d", counts["blocked"], counts["review"])
	}

	var att struct {
		Spaces []struct {
			HumanPending *int64 `json:"human_pending"`
		} `json:"spaces"`
	}
	if r := c.get(ctx, "/attention?space="+url.QueryEscape(s.ID), &att); r.ok() && len(att.Spaces) == 1 && att.Spaces[0].HumanPending != nil {
		fmt.Fprintf(&b, "\nNeeds you: %d", *att.Spaces[0].HumanPending)
	}

	var ib inboxPage
	if r := c.get(ctx, "/spaces/"+s.ID+"/inbox?limit=1", &ib); r.ok() {
		fmt.Fprintf(&b, "\nInbox: %d new · %d in progress · %d deferred", ib.Counts["new"], ib.Counts["in_progress"], ib.Counts["deferred"])
	}
	return reply{text: b.String(), markup: markup(
		[]botapi.InlineKeyboardButton{button("Inbox", listData(s.ID, "n", 0)), button("All spaces", "sl")})}
}

// spaceCallback handles "sp:<space id>" (switch and show the overview) and "sl" (the space list again).
func (h *Handler) spaceCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, data string) {
	if data == "sl" {
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, h.spacesReply(ctx, c))
		return
	}
	id, _ := strings.CutPrefix(data, "sp:")
	s, r, ok := h.space(ctx, c, id)
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	h.setCurrent(ctx, l.ID, s.ID)
	h.answer(ctx, q.ID, "")
	h.show(ctx, l.ChatID, q.Message, h.overview(ctx, c, s))
}
