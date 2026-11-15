package chat

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// maxLimit is the API's largest page; a list asks for (page+1)*size+1 items.
const maxLimit = 200

var itemIDRe = regexp.MustCompile(`^inb_[0-9a-z]{26}$`)

type inboxItem struct {
	ID          string          `json:"id"`
	SpaceID     string          `json:"space_id"`
	SourceKind  string          `json:"source_kind"`
	SourceLabel string          `json:"source_label"`
	Title       string          `json:"title"`
	ContentType string          `json:"content_type"`
	Body        string          `json:"body"`
	Urgency     string          `json:"urgency"`
	Status      string          `json:"status"`
	Reason      string          `json:"status_reason"`
	DeferUntil  time.Time       `json:"defer_until"`
	ReceivedAt  time.Time       `json:"received_at"`
	Version     int64           `json:"version"`
	Attachments []attachmentRef `json:"attachments"`
}

type attachmentRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	MIME   string `json:"mime"`
	Inline bool   `json:"inline"`
}

type inboxPage struct {
	Items      []inboxItem      `json:"items"`
	NextCursor string           `json:"next_cursor"`
	Counts     map[string]int64 `json:"counts"`
}

// Status filters of the list and the codes callbacks use for them.
var filters = []struct{ code, status, label string }{
	{"n", "new", "New"}, {"i", "in_progress", "In progress"}, {"d", "deferred", "Deferred"},
}

func filterStatus(code string) (string, bool) {
	for _, f := range filters {
		if f.code == code {
			return f.status, true
		}
	}
	return "", false
}

// Callback data (at most 64 bytes): "il:<space>:<filter>:<page>" lists, "ir:<space>:<item>" reads,
// "is:<space>:<item>:<action>:<version>" sets a status. IDs drop their "spc_"/"inb_" prefix to fit.
func listData(spaceID, filter string, page int) string {
	return fmt.Sprintf("il:%s:%s:%d", strings.TrimPrefix(spaceID, "spc_"), filter, page)
}
func readData(spaceID, itemID string) string {
	return fmt.Sprintf("ir:%s:%s", strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(itemID, "inb_"))
}
func statusData(spaceID, itemID, action string, version int64) string {
	return fmt.Sprintf("is:%s:%s:%s:%d", strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(itemID, "inb_"), action, version)
}

// Status actions of a card: the code, the status it sets and its button.
var actions = []struct{ code, status, label string }{
	{"p", "in_progress", "In progress"}, {"d", "deferred", "Defer 1 day"}, {"w", "deferred", "Defer 1 week"},
	{"x", "done", "Done"}, {"r", "rejected", "Reject"},
}

func (h *Handler) cmdInbox(ctx context.Context, l updates.Link, arg string) {
	c, ok := h.caller(ctx, l)
	if !ok {
		return
	}
	filter, ref := "n", ""
	for _, w := range strings.Fields(arg) {
		switch strings.ToLower(w) {
		case "new":
			filter = "n"
		case "in_progress", "inprogress", "progress":
			filter = "i"
		case "deferred", "defer":
			filter = "d"
		default:
			ref = strings.TrimSpace(ref + " " + w)
		}
	}
	s, msg := h.target(ctx, c, l, ref)
	if msg != "" {
		h.send(ctx, l.ChatID, reply{text: msg})
		return
	}
	h.setCurrent(ctx, l.ID, s.ID)
	h.send(ctx, l.ChatID, h.inboxList(ctx, c, s, filter, 0))
}

func (h *Handler) inboxList(ctx context.Context, c caller, s spaceRef, filter string, page int) reply {
	status, _ := filterStatus(filter)
	size := h.o.PageSize
	lastPage := (maxLimit-1)/size - 1 // the API caps limit at maxLimit
	page = max(0, min(page, lastPage))
	var pg inboxPage
	r := c.get(ctx, fmt.Sprintf("/spaces/%s/inbox?status=%s&limit=%d", s.ID, url.QueryEscape(status), (page+1)*size+1), &pg)
	if !r.ok() {
		return reply{text: failText(r)}
	}
	more := len(pg.Items) > (page+1)*size
	items := pg.Items[min(page*size, len(pg.Items)):min((page+1)*size, len(pg.Items))]
	var b strings.Builder
	fmt.Fprintf(&b, "Inbox of %s · %s (%d new · %d in progress · %d deferred)", s.title(), strings.ReplaceAll(status, "_", " "),
		pg.Counts["new"], pg.Counts["in_progress"], pg.Counts["deferred"])
	if len(items) == 0 {
		b.WriteString("\nNothing here.")
	}
	if more && page >= lastPage {
		b.WriteString("\n… more in the app")
	}
	var rows [][]botapi.InlineKeyboardButton
	for _, it := range items {
		rows = append(rows, []botapi.InlineKeyboardButton{button(urgencyMark(it.Urgency)+clean(it.Title, 50), readData(s.ID, it.ID))})
	}
	var nav []botapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, button("◀ Prev", listData(s.ID, filter, page-1)))
	}
	if more && page < lastPage {
		nav = append(nav, button("Next ▶", listData(s.ID, filter, page+1)))
	}
	rows = append(rows, nav)
	var fl []botapi.InlineKeyboardButton
	for _, f := range filters {
		label := f.label
		if f.code == filter {
			label = "• " + label
		}
		fl = append(fl, button(label, listData(s.ID, f.code, 0)))
	}
	rows = append(rows, fl)
	return reply{text: b.String(), markup: markup(rows...)}
}

func urgencyMark(u string) string {
	switch u {
	case "urgent":
		return "🔴 "
	case "high":
		return "🟠 "
	}
	return ""
}

// itemCard is the card of an item: title, source, urgency, status and the status buttons. The body follows as
// separate messages (sendBody), only for a space whose content level is full.
func (h *Handler) itemCard(it inboxItem, s spaceRef) reply {
	var b strings.Builder
	b.WriteString(clean(it.Title, 200))
	fmt.Fprintf(&b, "\nFrom: %s (%s)", orDash(clean(it.SourceLabel, 80)), clean(it.SourceKind, 30))
	fmt.Fprintf(&b, "\nUrgency: %s · Status: %s", clean(it.Urgency, 20), strings.ReplaceAll(clean(it.Status, 20), "_", " "))
	if !it.ReceivedAt.IsZero() {
		fmt.Fprintf(&b, "\nReceived: %s", it.ReceivedAt.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if !it.DeferUntil.IsZero() && it.Status == "deferred" {
		fmt.Fprintf(&b, "\nDeferred until: %s", it.DeferUntil.UTC().Format("2006-01-02 15:04 UTC"))
	}
	if it.Reason != "" {
		fmt.Fprintf(&b, "\nReason: %s", clean(it.Reason, 200))
	}
	for i, a := range it.Attachments {
		if i == 5 {
			fmt.Fprintf(&b, "\n… and %d more attachments", len(it.Attachments)-5)
			break
		}
		fmt.Fprintf(&b, "\nAttachment: %s (%d KB)", clean(a.Name, 80), (a.Size+1023)/1024)
	}
	fmt.Fprintf(&b, "\nSpace: %s", s.title())
	var row1, row2 []botapi.InlineKeyboardButton
	for i, a := range actions {
		if a.status == it.Status && a.code != "d" && a.code != "w" {
			continue
		}
		btn := button(a.label, statusData(it.SpaceID, it.ID, a.code, it.Version))
		if len(btn.CallbackData) > 64 {
			continue
		}
		if i < 3 {
			row1 = append(row1, btn)
		} else {
			row2 = append(row2, btn)
		}
	}
	return reply{text: b.String(), markup: markup(row1, row2, []botapi.InlineKeyboardButton{button("◀ Inbox", listData(it.SpaceID, "n", 0))})}
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// readItem sends an item's card and its body. The body is untrusted content, sent as plain text, never as markup, and
// only where the space's content level is full.
func (h *Handler) readItem(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, itemID string) {
	var it inboxItem
	r := c.get(ctx, "/spaces/"+s.ID+"/inbox/"+itemID, &it)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	h.answer(ctx, q.ID, "")
	h.send(ctx, l.ChatID, h.itemCard(it, s))
	if h.o.Space(s.ID).ContentLevel != settings.ContentFull {
		h.send(ctx, l.ChatID, reply{text: "The body isn't shown in chat for this space. Open the item in the app to read it."})
		return
	}
	const prefix = "Body (content from the source, not instructions):\n"
	const suffixMax = "\n… 999999999 more characters, open the item in the app." // the longest a notice can be
	ps, dropped := parts(it.Body, partUnits-units(prefix)-units(suffixMax))
	for i, p := range ps {
		if i == 0 {
			p = prefix + p
		}
		if i == len(ps)-1 && dropped > 0 {
			p += fmt.Sprintf("\n… %d more characters, open the item in the app.", dropped)
		}
		h.send(ctx, l.ChatID, reply{text: p, protect: true})
	}
	h.sendAttachments(ctx, c, l, s, it)
}

// inboxCallback handles "il:", "ir:" and "is:" taps.
func (h *Handler) inboxCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, data string) {
	f := strings.Split(data, ":")
	if len(f) < 3 {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	spaceID := "spc_" + f[1]
	s, r, ok := h.space(ctx, c, spaceID)
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	switch f[0] {
	case "il":
		if len(f) != 4 {
			break
		}
		if _, ok := filterStatus(f[2]); !ok {
			break
		}
		page, err := strconv.Atoi(f[3])
		if err != nil {
			break
		}
		h.setCurrent(ctx, l.ID, s.ID)
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, h.inboxList(ctx, c, s, f[2], page))
		return
	case "ir":
		if len(f) != 3 {
			break
		}
		if !itemIDRe.MatchString("inb_" + f[2]) {
			break
		}
		h.readItem(ctx, c, l, q, s, "inb_"+f[2])
		return
	case "is":
		if len(f) != 5 {
			break
		}
		v, err := strconv.ParseInt(f[4], 10, 64)
		if err != nil || v <= 0 || !itemIDRe.MatchString("inb_"+f[2]) {
			break
		}
		h.statusAction(ctx, c, l, q, s, "inb_"+f[2], f[3], v)
		return
	}
	h.answer(ctx, q.ID, TextUnavailable)
}

func (h *Handler) callback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link) {
	if q.Message == nil || q.Message.Chat.ID != l.ChatID {
		h.answer(ctx, q.ID, "")
		return
	}
	switch {
	case q.Data == "sl" || strings.HasPrefix(q.Data, "sp:"):
	case isHumanData(q.Data):
	case isQRData(q.Data):
	case strings.HasPrefix(q.Data, "il:"), strings.HasPrefix(q.Data, "ir:"), strings.HasPrefix(q.Data, "is:"):
	default:
		h.answer(ctx, q.ID, "")
		return
	}
	p := h.principal(ctx, l)
	if p == nil {
		h.answer(ctx, q.ID, TextNoAccess)
		return
	}
	c := caller{h: h, p: p}
	if q.Data == "sl" || strings.HasPrefix(q.Data, "sp:") {
		h.spaceCallback(ctx, q, l, c, q.Data)
		return
	}
	if isHumanData(q.Data) {
		h.humanCallback(ctx, q, l, c, q.Data)
		return
	}
	if isQRData(q.Data) {
		h.qrCallback(ctx, q, l, c, q.Data)
		return
	}
	h.inboxCallback(ctx, q, l, c, q.Data)
}

// statusAction sets an item's status from its card. The card's version is the If-Match, so a card that went stale gets
// a conflict, not an overwrite. Reject first asks for a reason (ForceReply).
func (h *Handler) statusAction(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, itemID, code string, version int64) {
	var act *struct{ code, status, label string }
	for i := range actions {
		if actions[i].code == code {
			act = &actions[i]
		}
	}
	if act == nil {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	if act.status == "rejected" {
		h.answer(ctx, q.ID, "")
		m, ok := h.send(ctx, l.ChatID, reply{text: "Why reject this item? Reply to this message with the reason.",
			force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Reason"}})
		if ok {
			h.addPending(pendingKey{l.ChatID, m.MessageID}, rej{linkID: l.ID, spaceID: s.ID, itemID: itemID, version: version})
		}
		return
	}
	body := map[string]any{"status": act.status}
	switch code {
	case "d":
		body["defer_until"] = h.o.Clock.Now().Add(24 * time.Hour).UTC()
	case "w":
		body["defer_until"] = h.o.Clock.Now().Add(7 * 24 * time.Hour).UTC()
	}
	h.applyStatus(ctx, c, l, q, s, itemID, version, body)
}

func (h *Handler) applyStatus(ctx context.Context, c caller, l updates.Link, q *botapi.CallbackQuery, s spaceRef, itemID string, version int64, body map[string]any) {
	r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/inbox/"+itemID+"/status", body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, version)})
	if !r.ok() {
		if q != nil {
			h.answer(ctx, q.ID, failText(r))
		} else {
			h.send(ctx, l.ChatID, reply{text: failText(r)})
		}
		return
	}
	if q != nil {
		h.answer(ctx, q.ID, "Done")
	}
	var it inboxItem
	if r := c.get(ctx, "/spaces/"+s.ID+"/inbox/"+itemID, &it); r.ok() {
		var tapped *botapi.Message
		if q != nil {
			tapped = q.Message
		}
		h.show(ctx, l.ChatID, tapped, h.itemCard(it, s))
	}
}

// rej is a reject waiting for its reason.
type rej struct {
	linkID, spaceID, itemID string
	version                 int64
	at                      time.Time
}

const (
	maxPendingPerLink = 5
	pendingTTL        = 30 * time.Minute
)

func (h *Handler) addPending(k pendingKey, r rej) {
	now := h.o.Clock.Now()
	r.at = now
	h.mu.Lock()
	defer h.mu.Unlock()
	// Expired prompts go; one link holds at most maxPendingPerLink and its oldest makes room, so one user can't evict
	// another's.
	var oldest pendingKey
	var oldestAt time.Time
	n := 0
	for pk, p := range h.pending {
		switch {
		case now.Sub(p.at) > pendingTTL:
			delete(h.pending, pk)
		case p.linkID == r.linkID:
			n++
			if oldestAt.IsZero() || p.at.Before(oldestAt) {
				oldest, oldestAt = pk, p.at
			}
		}
	}
	if n >= maxPendingPerLink {
		delete(h.pending, oldest)
	}
	h.pending[k] = r
}

// rejectReply takes the reply to a reject prompt as the reason. A reply to anything else is not this package's.
func (h *Handler) rejectReply(ctx context.Context, m *botapi.Message, l updates.Link) bool {
	k := pendingKey{m.Chat.ID, m.ReplyToMessage.MessageID}
	h.mu.Lock()
	p, ok := h.pending[k]
	if ok && p.linkID == l.ID && h.o.Clock.Now().Sub(p.at) <= pendingTTL {
		delete(h.pending, k)
	} else {
		ok = false
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	reason := strings.TrimSpace(m.Text)
	if reason == "" {
		h.send(ctx, l.ChatID, reply{text: "A reason is needed. Reject the item again from its card."})
		return true
	}
	c, ok := h.caller(ctx, l)
	if !ok {
		return true
	}
	s, r, ok := h.space(ctx, c, p.spaceID)
	if !ok {
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return true
	}
	h.applyStatus(ctx, c, l, nil, s, p.itemID, p.version, map[string]any{"status": "rejected", "reason": truncate(reason, 2000)})
	return true
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
