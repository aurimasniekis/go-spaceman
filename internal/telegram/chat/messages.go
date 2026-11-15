package chat

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Messages (plan §2.5, TG-8b).
//
//   - Messages to operators: /messages lists the ones still waiting; a card offers Reply (a ForceReply prompt, then
//     reply-message), Acknowledge and Resolve. A Telegram reply to a notifier card of such a message is a reply to it
//     too (Options.ResolveMessage).
//   - Talk to control: /control puts the link in control mode for a space; while it is on, a plain text message is not
//     captured into the inbox but offered as a draft with a "Send to control" button, which sends it as a message to
//     the space's control run (send-message, as the user, expecting a reply). Control's reply is a message to the
//     operators and reaches the chat the way any such message does (the notifier's card). Media, forwards and albums
//     still go to the inbox. /control off ends the mode, as does 1 hour without use.

// controlModeTTL is how long control mode lasts without being used.
const controlModeTTL = time.Hour

// qrState is the state of this file and prompts of the queue and run cards; zero value is ready, bounded per link.
type qrState struct {
	mu      sync.Mutex
	prompts map[pendingKey]replyPrompt
	drafts  map[pendingKey]controlDraft
	confirm map[pendingKey]confirmation
	control map[string]controlMode // link ID -> mode
}

type replyPrompt struct {
	linkID, spaceID, messageID string
	at                         time.Time
}

type controlDraft struct {
	linkID, spaceID, text string
	at                    time.Time
}

// confirmation is what a Confirm button on a stop or incident confirmation message is bound to: the tap only acts when
// it comes from that message, for the link it was sent to, for the run or task it names, within pendingTTL, once.
type confirmation struct {
	linkID, kind, spaceID, id, decision string
	version                             int64
	at                                  time.Time
}

type controlMode struct {
	spaceID string
	at      time.Time
}

// maxQRPerLink bounds the prompts and drafts one link holds; its oldest makes room.
const maxQRPerLink = 5

func (h *Handler) qrAddPrompt(k pendingKey, p replyPrompt) {
	now := h.o.Clock.Now()
	p.at = now
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	if h.qr.prompts == nil {
		h.qr.prompts = map[pendingKey]replyPrompt{}
	}
	var oldest pendingKey
	var oldestAt time.Time
	n := 0
	for pk, v := range h.qr.prompts {
		switch {
		case now.Sub(v.at) > pendingTTL:
			delete(h.qr.prompts, pk)
		case v.linkID == p.linkID:
			n++
			if oldestAt.IsZero() || v.at.Before(oldestAt) {
				oldest, oldestAt = pk, v.at
			}
		}
	}
	if n >= maxQRPerLink {
		delete(h.qr.prompts, oldest)
	}
	h.qr.prompts[k] = p
}

func (h *Handler) qrAddDraft(k pendingKey, d controlDraft) {
	now := h.o.Clock.Now()
	d.at = now
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	if h.qr.drafts == nil {
		h.qr.drafts = map[pendingKey]controlDraft{}
	}
	var oldest pendingKey
	var oldestAt time.Time
	n := 0
	for pk, v := range h.qr.drafts {
		switch {
		case now.Sub(v.at) > pendingTTL:
			delete(h.qr.drafts, pk)
		case v.linkID == d.linkID:
			n++
			if oldestAt.IsZero() || v.at.Before(oldestAt) {
				oldest, oldestAt = pk, v.at
			}
		}
	}
	if n >= maxQRPerLink {
		delete(h.qr.drafts, oldest)
	}
	h.qr.drafts[k] = d
}

func (h *Handler) qrAddConfirm(k pendingKey, c confirmation) {
	now := h.o.Clock.Now()
	c.at = now
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	if h.qr.confirm == nil {
		h.qr.confirm = map[pendingKey]confirmation{}
	}
	var oldest pendingKey
	var oldestAt time.Time
	n := 0
	for pk, v := range h.qr.confirm {
		switch {
		case now.Sub(v.at) > pendingTTL:
			delete(h.qr.confirm, pk)
		case v.linkID == c.linkID:
			n++
			if oldestAt.IsZero() || v.at.Before(oldestAt) {
				oldest, oldestAt = pk, v.at
			}
		}
	}
	if n >= maxQRPerLink {
		delete(h.qr.confirm, oldest)
	}
	h.qr.confirm[k] = c
}

// takeConfirm consumes the confirmation of a tapped message when it is the link's and matches kind, space, ID, decision
// and version, and has not expired. Another link's entry is left alone; any other mismatch or expiry consumes it.
func (h *Handler) takeConfirm(k pendingKey, linkID, kind, spaceID, id, decision string, version int64) (confirmation, bool) {
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	c, ok := h.qr.confirm[k]
	if !ok || c.linkID != linkID {
		return confirmation{}, false
	}
	delete(h.qr.confirm, k)
	if h.o.Clock.Now().Sub(c.at) > pendingTTL {
		return confirmation{}, false
	}
	if c.kind != kind || c.spaceID != spaceID || c.id != id || c.decision != decision || c.version != version {
		return confirmation{}, false
	}
	return c, true
}

// takeDraft removes and returns the draft of a message for the link (if it is the link's and not expired).
func (h *Handler) takeDraft(k pendingKey, linkID string) (controlDraft, bool) {
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	d, ok := h.qr.drafts[k]
	if !ok || d.linkID != linkID {
		return controlDraft{}, false
	}
	delete(h.qr.drafts, k)
	return d, h.o.Clock.Now().Sub(d.at) <= pendingTTL
}

// controlSpace returns the space control mode is on for (refreshing its time), or "".
func (h *Handler) controlSpace(linkID string) string {
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	m, ok := h.qr.control[linkID]
	if !ok {
		return ""
	}
	now := h.o.Clock.Now()
	if now.Sub(m.at) > controlModeTTL {
		delete(h.qr.control, linkID)
		return ""
	}
	m.at = now
	h.qr.control[linkID] = m
	return m.spaceID
}

func (h *Handler) setControl(linkID, spaceID string) {
	h.qr.mu.Lock()
	defer h.qr.mu.Unlock()
	if h.qr.control == nil {
		h.qr.control = map[string]controlMode{}
	}
	if spaceID == "" {
		delete(h.qr.control, linkID)
		return
	}
	h.qr.control[linkID] = controlMode{spaceID: spaceID, at: h.o.Clock.Now()}
}

// --- /control and its drafts ---

const (
	TextControlOff   = "Control mode is off. Plain messages go to the inbox again."
	TextControlGone  = "That draft expired. Send the message again."
	controlSubjectRn = 80
)

func (h *Handler) cmdControl(ctx context.Context, l updates.Link, arg string) {
	if strings.EqualFold(strings.TrimSpace(arg), "off") {
		h.setControl(l.ID, "")
		h.send(ctx, l.ChatID, reply{text: TextControlOff})
		return
	}
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
	h.setControl(l.ID, s.ID)
	h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("Control mode for %s. Your next text messages become drafts you can send to its control run; images, files and forwards still go to the inbox. Control's replies come back here. /control off ends it.", s.title())})
}

// controlText turns a plain text message into a draft while control mode is on. It reports whether it took the message;
// anything else (media, a forward, an album, a command) is left to capture.
func (h *Handler) controlText(ctx context.Context, m *botapi.Message, l updates.Link) bool {
	spaceID := h.controlSpace(l.ID)
	if spaceID == "" || m.MediaGroupID != "" {
		return false
	}
	if pin := pinnedOf(ctx); pin != "" && pin != spaceID {
		return false // written in another space's topic: that topic's space decides, not the control mode of this one
	}
	p, ok := extract(m)
	if !ok || p.origin != "" || len(p.files) > 0 || p.text == "" {
		return false
	}
	c, ok := h.caller(ctx, l)
	if !ok {
		return true
	}
	s, r, ok := h.space(ctx, c, spaceID)
	if !ok {
		h.setControl(l.ID, "")
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return true
	}
	preview := clean(p.text, 300)
	d, sent := h.send(ctx, l.ChatID, reply{text: fmt.Sprintf("Send to the control run of %s?\n\n%s", s.title(), preview),
		markup: markup([]botapi.InlineKeyboardButton{button("Send to control", "cs:1"), button("Cancel", "cx:1")})})
	if sent {
		h.qrAddDraft(pendingKey{l.ChatID, d.MessageID}, controlDraft{linkID: l.ID, spaceID: s.ID, text: p.text})
	}
	return true
}

func (h *Handler) controlCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, f []string) {
	k := pendingKey{q.Message.Chat.ID, q.Message.MessageID}
	d, ok := h.takeDraft(k, l.ID)
	if !ok {
		h.answer(ctx, q.ID, TextControlGone)
		h.show(ctx, l.ChatID, q.Message, reply{text: TextControlGone})
		return
	}
	if f[0] == "cx" {
		h.answer(ctx, q.ID, "Cancelled")
		h.show(ctx, l.ChatID, q.Message, reply{text: "Cancelled. Nothing was sent."})
		return
	}
	s, r, ok := h.space(ctx, c, d.spaceID)
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	subject := clean(strings.SplitN(strings.TrimSpace(d.text), "\n", 2)[0], controlSubjectRn)
	if subject == "" {
		subject = "Message from Telegram"
	}
	// The Idempotency-Key names the draft message, so a retried tap sends once.
	key := fmt.Sprintf("tg-control-%d-%d", k.chat, k.msg)
	r = c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/messages", map[string]any{"to": "control", "subject": subject, "body": truncate(d.text, 100000), "request": true},
		map[string]string{"Idempotency-Key": key})
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		h.show(ctx, l.ChatID, q.Message, reply{text: failText(r)})
		return
	}
	h.answer(ctx, q.ID, "Sent")
	h.show(ctx, l.ChatID, q.Message, reply{text: fmt.Sprintf("Sent to control in %s. Its reply will arrive here.", s.title())})
}

// --- messages to operators ---

type opMessage struct {
	ID         string    `json:"id"`
	SpaceID    string    `json:"space_id"`
	FromKind   string    `json:"from_kind"`
	FromLabel  string    `json:"from_label"`
	ToKind     string    `json:"to_kind"`
	Kind       string    `json:"kind"`
	Subject    string    `json:"subject"`
	Body       string    `json:"body"`
	State      string    `json:"state"`
	Resolution string    `json:"resolution"`
	CreatedAt  time.Time `json:"created_at"`
	Version    int64     `json:"version"`
}

// waiting is a message to operators nobody has acknowledged yet; open also covers the acknowledged ones, which still
// need a Resolve (and a Reply, for a run's request).
func (m opMessage) waiting() bool {
	return m.ToKind == "operators" && (m.State == "stored" || m.State == "available")
}

func (m opMessage) open() bool {
	return m.waiting() || (m.ToKind == "operators" && m.State == "acknowledged")
}

// Callback data: "ml:<space>:<page>" lists, "mk:<space>:<msg>" opens, "ma:…:<version>" acknowledges, "mv:…:<version>"
// resolves, "mr:<space>:<msg>" asks for a reply text.
func msgData(verb, spaceID, msgID string, version int64) string {
	d := fmt.Sprintf("%s:%s:%s", verb, strings.TrimPrefix(spaceID, "spc_"), strings.TrimPrefix(msgID, "msg_"))
	if version > 0 {
		d += ":" + strconv.FormatInt(version, 10)
	}
	return d
}

func (h *Handler) cmdMessages(ctx context.Context, l updates.Link, arg string) {
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
	h.send(ctx, l.ChatID, h.messagesList(ctx, c, s, 0))
}

// messagesListLimit is how many recent messages /messages reads; a longer history is cut and the list says so.
const messagesListLimit = 200

func (h *Handler) messagesList(ctx context.Context, c caller, s spaceRef, page int) reply {
	var out struct {
		Items            []opMessage `json:"items"`
		OperatorsPending int64       `json:"operators_pending"`
	}
	r := c.get(ctx, fmt.Sprintf("/spaces/%s/messages?limit=%d", s.ID, messagesListLimit), &out)
	if !r.ok() {
		return reply{text: failText(r)}
	}
	// Waiting messages first, then the acknowledged ones that still need resolving.
	var shown []opMessage
	nWait := 0
	for _, m := range out.Items {
		if m.waiting() && m.SpaceID == s.ID {
			shown = append(shown, m)
			nWait++
		}
	}
	for _, m := range out.Items {
		if m.ToKind == "operators" && m.State == "acknowledged" && m.SpaceID == s.ID {
			shown = append(shown, m)
		}
	}
	const size = 8
	last := max(0, (len(shown)-1)/size)
	page = max(0, min(page, last))
	var b strings.Builder
	fmt.Fprintf(&b, "Messages to operators in %s (%d waiting)", s.title(), out.OperatorsPending)
	if len(shown) == 0 {
		b.WriteString("\nNothing waiting or acknowledged.")
	} else {
		fmt.Fprintf(&b, "\nWaiting: %d · Acknowledged, not resolved: %d", nWait, len(shown)-nWait)
	}
	if len(out.Items) >= messagesListLimit {
		fmt.Fprintf(&b, "\nOnly the %d most recent messages are read; older ones are in the app.", messagesListLimit)
	}
	var rows [][]botapi.InlineKeyboardButton
	lo := min(page*size, len(shown))
	for i, m := range shown[lo:min((page+1)*size, len(shown))] {
		mark := "⏳ "
		if lo+i >= nWait {
			mark = "✔ "
		}
		rows = append(rows, []botapi.InlineKeyboardButton{button(mark+clean(m.Subject, 50), msgData("mk", s.ID, m.ID, 0))})
	}
	var nav []botapi.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, button("◀ Prev", fmt.Sprintf("ml:%s:%d", strings.TrimPrefix(s.ID, "spc_"), page-1)))
	}
	if page < last {
		nav = append(nav, button("Next ▶", fmt.Sprintf("ml:%s:%d", strings.TrimPrefix(s.ID, "spc_"), page+1)))
	}
	rows = append(rows, nav)
	return reply{text: b.String(), markup: markup(rows...)}
}

func (h *Handler) messageCard(m opMessage, s spaceRef) reply {
	var b strings.Builder
	b.WriteString(clean(m.Subject, 200))
	fmt.Fprintf(&b, "\nFrom: %s (%s) · %s", orDash(clean(m.FromLabel, 80)), clean(m.FromKind, 20), clean(m.Kind, 20))
	fmt.Fprintf(&b, "\nState: %s", clean(m.State, 20))
	// The resolution is free text written by a person or an agent: content, shown only at the full level.
	if m.Resolution != "" && h.o.Space(s.ID).ContentLevel == settings.ContentFull {
		fmt.Fprintf(&b, "\nResolution: %s", clean(m.Resolution, 200))
	}
	fmt.Fprintf(&b, "\nSpace: %s", s.title())
	var row []botapi.InlineKeyboardButton
	if m.open() {
		if m.FromKind == "run" {
			row = append(row, button("Reply", msgData("mr", s.ID, m.ID, 0)))
		}
		if m.waiting() {
			row = append(row, button("Acknowledge", msgData("ma", s.ID, m.ID, m.Version)))
		}
		row = append(row, button("Resolve", msgData("mv", s.ID, m.ID, m.Version)))
	}
	return reply{text: b.String(), markup: markup(row, []botapi.InlineKeyboardButton{button("◀ Messages", fmt.Sprintf("ml:%s:0", strings.TrimPrefix(s.ID, "spc_")))})}
}

func (h *Handler) getMessage(ctx context.Context, c caller, s spaceRef, id string) (opMessage, result) {
	var m opMessage
	r := c.get(ctx, "/spaces/"+s.ID+"/messages/"+id, &m)
	if r.ok() && (m.ID != id || m.SpaceID != s.ID) {
		r.status = http.StatusNotFound
	}
	return m, r
}

// messagesCallback handles the "m…" taps.
func (h *Handler) messagesCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, f []string) {
	if len(f) < 3 {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	s, r, ok := h.space(ctx, c, "spc_"+f[1])
	if !ok {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	if f[0] == "ml" {
		page, err := strconv.Atoi(f[2])
		if err != nil || len(f) != 3 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		h.setCurrent(ctx, l.ID, s.ID)
		h.answer(ctx, q.ID, "")
		h.show(ctx, l.ChatID, q.Message, h.messagesList(ctx, c, s, page))
		return
	}
	id := "msg_" + f[2]
	if !msgIDRe.MatchString(id) {
		h.answer(ctx, q.ID, TextUnavailable)
		return
	}
	m, r := h.getMessage(ctx, c, s, id)
	if !r.ok() {
		h.answer(ctx, q.ID, failText(r))
		return
	}
	switch f[0] {
	case "mk":
		h.answer(ctx, q.ID, "")
		h.send(ctx, l.ChatID, h.messageCard(m, s))
		h.sendMessageBody(ctx, l, s, m)
	case "mr":
		if !m.open() || m.FromKind != "run" {
			h.answer(ctx, q.ID, TextStale)
			h.show(ctx, l.ChatID, q.Message, h.messageCard(m, s))
			return
		}
		h.answer(ctx, q.ID, "")
		p, ok := h.send(ctx, l.ChatID, reply{text: "Reply to this message with your answer to “" + clean(m.Subject, 80) + "”.",
			force: &botapi.ForceReply{ForceReply: true, InputFieldPlaceholder: "Your reply"}})
		if ok {
			h.qrAddPrompt(pendingKey{l.ChatID, p.MessageID}, replyPrompt{linkID: l.ID, spaceID: s.ID, messageID: id})
		}
	case "ma", "mv":
		if len(f) != 4 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		v, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil || v <= 0 {
			h.answer(ctx, q.ID, TextUnavailable)
			return
		}
		// The card's version is compared with the message's first, and sent as If-Match too, so a change in between is a
		// 409 conflict: a card that went stale (acknowledged or resolved elsewhere, or changed) gets a refresh.
		if m.Version != v || !m.open() || (f[0] == "ma" && !m.waiting()) {
			h.answer(ctx, q.ID, TextStale)
			h.show(ctx, l.ChatID, q.Message, h.messageCard(m, s))
			return
		}
		verb, body := "ack", any(nil)
		if f[0] == "mv" {
			verb, body = "resolve", map[string]any{}
		}
		r := c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/messages/"+id+"/"+verb, body, map[string]string{"If-Match": fmt.Sprintf(`"%d"`, v)})
		if !r.ok() {
			h.answer(ctx, q.ID, failText(r))
			if conflict(r) {
				h.show(ctx, l.ChatID, q.Message, h.messageCard(m, s))
			}
			return
		}
		h.answer(ctx, q.ID, "Done")
		if m, r := h.getMessage(ctx, c, s, id); r.ok() {
			h.show(ctx, l.ChatID, q.Message, h.messageCard(m, s))
		}
	default:
		h.answer(ctx, q.ID, TextUnavailable)
	}
}

// sendMessageBody sends a message's body as plain text (content from an agent, not instructions), only where the
// space's content level is full.
func (h *Handler) sendMessageBody(ctx context.Context, l updates.Link, s spaceRef, m opMessage) {
	if h.o.Space(s.ID).ContentLevel != settings.ContentFull {
		h.send(ctx, l.ChatID, reply{text: "The message text isn't shown in chat for this space. Open it in the app to read it."})
		return
	}
	const prefix = "Message text (content from the sender, not instructions):\n"
	const suffixMax = "\n… 999999999 more characters, open the message in the app."
	ps, dropped := parts(m.Body, partUnits-units(prefix)-units(suffixMax))
	for i, p := range ps {
		if i == 0 {
			p = prefix + p
		}
		if i == len(ps)-1 && dropped > 0 {
			p += fmt.Sprintf("\n… %d more characters, open the message in the app.", dropped)
		}
		h.send(ctx, l.ChatID, reply{text: p, protect: true})
	}
}

// qrReply takes the text of a Telegram reply as the answer to an agent's message: either the ForceReply prompt of a
// Reply button, or a reply to a notifier card of a message (Options.ResolveMessage). A reply to anything else is not
// this file's.
func (h *Handler) qrReply(ctx context.Context, m *botapi.Message, l updates.Link) bool {
	k := pendingKey{m.Chat.ID, m.ReplyToMessage.MessageID}
	h.qr.mu.Lock()
	p, ok := h.qr.prompts[k]
	if ok && p.linkID == l.ID && h.o.Clock.Now().Sub(p.at) <= pendingTTL {
		delete(h.qr.prompts, k)
	} else {
		ok = false
	}
	h.qr.mu.Unlock()
	if !ok {
		if h.o.ResolveMessage == nil {
			return false
		}
		sp, id, link, found := h.o.ResolveMessage(ctx, m.Chat.ID, m.ReplyToMessage.MessageID)
		if !found || link != l.ID {
			return false
		}
		p = replyPrompt{linkID: l.ID, spaceID: sp, messageID: id}
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		h.send(ctx, l.ChatID, reply{text: "A reply needs text. Use Reply on the message's card to try again."})
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
	r = c.do(ctx, http.MethodPost, "/spaces/"+s.ID+"/messages/"+p.messageID+"/reply", map[string]any{"body": truncate(text, 100000)}, nil)
	if !r.ok() {
		h.send(ctx, l.ChatID, reply{text: failText(r)})
		return true
	}
	h.send(ctx, l.ChatID, reply{text: "Reply sent."})
	return true
}

// isQRData reports whether callback data is one of this file's, queue.go's or runs.go's: two lowercase letters and a
// colon, the first one q (queue), r (runs), m (messages) or c (control drafts).
func isQRData(d string) bool {
	if len(d) < 3 || d[2] != ':' || d[1] < 'a' || d[1] > 'z' {
		return false
	}
	switch d[0] {
	case 'q':
		return strings.Contains("lksmrij", d[1:2])
	case 'r':
		return strings.Contains("lksy", d[1:2])
	case 'm':
		return strings.Contains("lkarv", d[1:2])
	case 'c':
		return strings.Contains("sx", d[1:2])
	}
	return false
}

func (h *Handler) qrCallback(ctx context.Context, q *botapi.CallbackQuery, l updates.Link, c caller, data string) {
	f := strings.Split(data, ":")
	switch f[0][0] {
	case 'q':
		h.queueCallback(ctx, q, l, c, f)
	case 'r':
		h.runsCallback(ctx, q, l, c, f)
	case 'm':
		h.messagesCallback(ctx, q, l, c, f)
	case 'c':
		h.controlCallback(ctx, q, l, c, f)
	}
}
