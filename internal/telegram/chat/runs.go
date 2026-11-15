package chat

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// /runs (plan §2.5, TG-8b): the live runs of a space and a run card. Stop needs the run:control permission, which the
// API enforces for the link's principal, and a confirmation. There is no launching, input or terminal from the chat.

type runInfo struct {
	ID        string     `json:"id"`
	SpaceID   string     `json:"space_id"`
	Kind      string     `json:"kind"`
	Role      string     `json:"role"`
	Desired   string     `json:"desired"`
	Observed  string     `json:"observed"`
	Status    string     `json:"status"`
	EndReason string     `json:"end_reason"`
	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at"`
}

func (r runInfo) live() bool {
	return slices.Contains([]string{"pending", "starting", "running", "stopping"}, r.Observed)
}

func (r runInfo) label() string {
	if r.Role != "" {
		return clean(r.Role, 40) + " (" + clean(r.Kind, 20) + ")"
	}
	return clean(r.Kind, 20)
}

// Callback data: "rl:<space>:<page>" lists, "rk:<space>:<run>" opens a run, "rs:…" asks to stop it, "ry:…" stops it.
func runData(verb, spaceID, runID string) string {
	return fmt.Sprintf("%s:%s:%s", verb, strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(runID, "run_"))
}

func (h *Handler) cmdRuns(ctx context.Context, l updates.Link, arg string) {
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
	h.send(ctx, l.ChatID, h.runsList(ctx, c, s, 0))
}

func (h *Handler) loadRuns(ctx context.Context, c caller, spaceID string) ([]runInfo, result) {
	var out struct {
		Items []runInfo `json:"items"`
	}
	r := c.get(ctx, "/spaces/"+spaceID+"/runs?limit=200", &out)
	var live []runInfo
	for _, ru := range out.Items {
		if ru.live() && ru.SpaceID == spaceID {
			live = append(live, ru)
		}
	}
	return live, r
}

func (h *Handler) runsList(ctx context.Context, c caller, s spaceRef, page int) reply {
	live, r := h.loadRuns(ctx, c, s.ID)
	if !r.ok() {
		return reply{text: failText(r)}
	}
	const size = 8
	last := max(0, (len(live)-1)/size)
	page = max(0, min(page, last))
	var b strings.Builder
	fmt.Fprintf(&b, "Runs of %s (%d live)", s.title(), len(live))
	if len(live) == 0 {
		b.WriteString("\nNo live runs.")
	}
	var rows [][]botapi.InlineKeyboardButton
	for _, ru := range live[min(page*size, len(live)):min((page+1)*size, len(live))] {
		rows = append(rows, []botapi.InlineKeyboardButton{button(runState(ru)+ru.label(), runData("rk", s.ID, ru.ID))})
	}
	var nav []botapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, button("◀ Prev", fmt.Sprintf("rl:%s:%d", strings.TrimPrefix(s.ID, "spc_"), page-1)))
	}
	if page < last {
		nav = append(nav, button("Next ▶", fmt.Sprintf("rl:%s:%d", strings.TrimPrefix(s.ID, "spc_"), page+1)))
	}
	rows = append(rows, nav)
	return reply{text: b.String(), markup: markup(rows...)}
}

func runState(r runInfo) string {
	switch {
	case r.Observed != "running":
		return "◌ "
	case r.Status == "working":
		return "▶ "
	case r.Status == "blocked":
		return "⛔ "
	}
	return "● "
}

func (h *Handler) runCard(r runInfo, s spaceRef) reply {
	var b strings.Builder
	b.WriteString(r.label())
	fmt.Fprintf(&b, "\nKind: %s", clean(r.Kind, 20))
	fmt.Fprintf(&b, "\nDesired: %s · Observed: %s", clean(r.Desired, 20), clean(r.Observed, 20))
	if r.Status != "" {
		fmt.Fprintf(&b, "\nStatus: %s", clean(r.Status, 20))
	}
	if t := r.CreatedAt; !t.IsZero() {
		fmt.Fprintf(&b, "\nStarted: %s (%s ago)", t.UTC().Format("2006-01-02 15:04 UTC"), age(h.o.Clock.Now().Sub(t)))
	}
	fmt.Fprintf(&b, "\nSpace: %s", s.title())
	var row []botapi.InlineKeyboardButton
	if r.live() && r.Observed != "stopping" {
		row = append(row, button("Stop…", runData("rs", s.ID, r.ID)))
	}
	return reply{text: b.String(), markup: markup(row, []botapi.InlineKeyboardButton{button("◀ Runs", fmt.Sprintf("rl:%s:0", strings.TrimPrefix(s.ID, "spc_")))})}
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + " min"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + " days"
}

func (h *Handler) getRun(ctx context.Context, c caller, s spaceRef, id string) (runInfo, result) {
	var ru runInfo
	r := c.get(ctx, "/spaces/"+s.ID+"/runs/"+id, &ru)
	if r.ok() && (ru.ID != id || ru.SpaceID != s.ID) {
		r.status = http.StatusNotFound
	}
	return ru, r
}

// runsCallback handles the "r…" taps.
func (h *Handler) runsCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, f []string) {
	if len(f) != 3 {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	s, r, ok := h.space(ctx, c, "spc_"+f[1])
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	if f[0] == "rl" {
		page, err := strconv.Atoi(f[2])
		if err != nil {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		h.setCurrent(l.ID, s.ID)
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, h.runsList(ctx, c, s, page))
		return
	}
	id := "run_" + f[2]
	if !runIDRe.MatchString(id) {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	ru, r := h.getRun(ctx, c, s, id)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	switch f[0] {
	case "rk":
		h.answer(ctx, q.ID, "")
		h.send(ctx, l.ChatID, h.runCard(ru, s))
	case "rs":
		if !ru.live() || ru.Observed == "stopping" {
			h.answer(ctx, q.ID, "That run has already ended or is stopping.")
			h.show(ctx, l.ChatID, q.Message, h.runCard(ru, s))
			return
		}
		h.answer(ctx, q.ID, "")
		warn := ""
		if ru.Kind == "control" {
			warn = "\nThis is the control run: it coordinates the space."
		}
		h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("Stop %s in %s? The run is interrupted and its session can be resumed.%s", ru.label(), s.title(), warn),
			markup: markup([]botapi.InlineKeyboardButton{button("Stop run", runData("ry", s.ID, id)), button("Cancel", runData("rk", s.ID, id))})})
	case "ry":
		if !ru.live() || ru.Observed == "stopping" {
			h.answer(ctx, q.ID, "That run has already ended or is stopping.")
			h.show(ctx, l.ChatID, q.Message, h.runCard(ru, s))
			return
		}
		// The API enforces run:control for the link's principal; a viewer link is refused here.
		r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/runs/"+id+"/stop", map[string]any{"mode": "interrupt", "reason": "stopped from Telegram"}, nil)
		if !r.ok() {
			h.answer(ctx, q.ID, failText(r))
			return
		}
		h.answer(ctx, q.ID, "Stopping")
		if ru, r := h.getRun(ctx, c, s, id); r.ok() {
			h.show(ctx, l.ChatID, q.Message, h.runCard(ru, s))
		}
	default:
		h.answer(ctx, q.ID, TextUnavailable)
	}
}
