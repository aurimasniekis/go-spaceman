// Package chat holds the bot's chat surfaces for a linked user (ADR-0083, plan §2.1 and §2.2, TG-8a): /spaces and
// /space to switch and see an overview, /inbox to list, read and set the status of inbox items.
//
// Every command acts as the link's Telegram principal and goes through the API's in-process handler
// (api.Deps.InProcessHandler), so each read and write passes the same rule, authorization, validation and audit as over
// HTTP. A space is shown only when the user may read it (the API says so) and Telegram may see it (the space's
// settings: enabled, content level other than off, TG-3c). A space or item that is hidden, missing or forbidden is
// answered with the same text, so a user can't tell them apart.
package chat

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/settings"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/updates"
)

// Texts the chat answers with.
const (
	TextUnavailable = "That isn't available. Use /spaces to see your spaces."
	TextNoSpaces    = "You have no spaces visible in Telegram. A space shows up here once Telegram is enabled for it and you can read it."
	TextPickSpace   = "Pick a space first with /spaces."
	TextFailed      = "Something went wrong. Try again in a moment."
	TextStale       = "That item changed since this card was shown. Reopen it from /inbox."
	TextNoAccess    = "Your link can't do that. Ask an administrator, or use the web UI."
	apiTimeout      = 20 * time.Second
	maxBodyParts    = 3
	// partUnits is the size of one body message in UTF-16 code units (the Bot API's measure; its limit is 4096), prefix
	// and suffix included.
	partUnits = 3500
	// callbackAnswerUnits is the Bot API's limit for a callback answer's text (200 UTF-16 units); the detail of a
	// problem is cut well under it so the "Not accepted: " lead fits.
	callbackAnswerUnits = 200
	detailRunes         = 150
)

// Bot is the part of the Bot API the commands use. *botapi.Client satisfies it.
type Bot interface {
	SendMessage(ctx context.Context, p botapi.SendMessageParams) (botapi.Message, error)
	EditMessageText(ctx context.Context, p botapi.EditMessageTextParams) (botapi.Message, error)
	AnswerCallbackQuery(ctx context.Context, p botapi.AnswerCallbackQueryParams) error
	// Capture: files a user sent come down with GetFile and DownloadFile; attachments of an item go back as uploads.
	GetFile(ctx context.Context, fileID string) (botapi.File, error)
	DownloadFile(ctx context.Context, filePath string, w io.Writer) (int64, error)
	SendPhoto(ctx context.Context, p botapi.SendPhotoParams) (botapi.Message, error)
	SendDocument(ctx context.Context, p botapi.SendDocumentParams) (botapi.Message, error)
}

// Options configure a Handler.
type Options struct {
	// API serves the operations in process (api.Deps.InProcessHandler).
	API http.Handler
	// Principal builds the principal of a link for each update (auth.Service.TelegramPrincipal), so a revoked link or a
	// disabled user acts for nobody at once.
	Principal func(ctx context.Context, linkID string) (*auth.Principal, error)
	// Space returns a space's Telegram settings (settings.Service.Space).
	Space func(spaceID string) settings.Space
	// Bot returns the live bot client (nil: nothing is sent).
	Bot func() Bot

	// Allow takes one unit of a link's inbound limit of that kind (updates.Limiter.Allow); nil: no limit. Capture costs
	// one KindInboxItem per item and one KindDownload per file moved in either direction.
	Allow func(linkID string, k updates.Kind) bool
	// Content opens an attachment's stored bytes. The caller has already read the attachment's metadata through the
	// API as the link's principal, which is the authorization; nil: attachments are not sent to Telegram.
	Content func(ctx context.Context, spaceID, attachmentID string) (io.ReadSeekCloser, error)
	// AlbumQuiet is how long an album waits for its next part before it becomes one item (default 1.5 s).
	AlbumQuiet time.Duration

	PageSize int // inbox items per page (default 8)
	Clock    clock.Clock
	Log      *slog.Logger
}

// Handler implements updates.Handler for the commands of this package.
type Handler struct {
	o Options

	mu      sync.Mutex
	current map[string]string   // link ID -> space ID chosen with /spaces or /space
	pending map[pendingKey]rej  // a reject reason being asked for
	albums  map[albumKey]*album // media groups still collecting parts
}

var _ updates.Handler = (*Handler)(nil)

type pendingKey struct {
	chat int64
	msg  int
}

// New returns a Handler.
func New(o Options) *Handler {
	if o.PageSize <= 0 {
		o.PageSize = 8
	}
	if o.AlbumQuiet <= 0 {
		o.AlbumQuiet = 1500 * time.Millisecond
	}
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	return &Handler{o: o, current: map[string]string{}, pending: map[pendingKey]rej{}, albums: map[albumKey]*album{}}
}

// Handle routes a linked user's update: commands, button taps, the reply to a reject-reason prompt, and capture: any
// other message with text, a photo or a file becomes an inbox item in the link's current space (capture.go).
func (h *Handler) Handle(ctx context.Context, u botapi.Update, l updates.Link) {
	switch {
	case u.CallbackQuery != nil:
		h.callback(ctx, u.CallbackQuery, l)
	case u.Message != nil:
		m := u.Message
		if cmd, arg, ok := command(m.Text); ok {
			switch cmd {
			case "spaces":
				h.cmdSpaces(ctx, l)
			case "space":
				h.cmdSpace(ctx, l, arg)
			case "inbox":
				h.cmdInbox(ctx, l, arg)
			}
			return
		}
		if m.ReplyToMessage != nil && h.rejectReply(ctx, m, l) {
			return
		}
		h.capture(ctx, m, l)
	}
}

// command splits "/name@bot args" into its parts; ok is false for text that is not a command.
func command(text string) (name, arg string, ok bool) {
	t := strings.TrimSpace(text)
	if !strings.HasPrefix(t, "/") {
		return "", "", false
	}
	head, arg, _ := strings.Cut(t[1:], " ")
	if i := strings.IndexByte(head, '@'); i >= 0 {
		head = head[:i]
	}
	return strings.ToLower(head), strings.TrimSpace(arg), head != ""
}

// --- sending ---

func (h *Handler) bot() Bot {
	if h.o.Bot == nil {
		return nil
	}
	return h.o.Bot()
}

type reply struct {
	text   string
	markup *botapi.InlineKeyboardMarkup
	force  *botapi.ForceReply
	// protect sets protect_content (no forwarding or saving), for message bodies (ADR-0083 decision 13).
	protect bool
}

func (h *Handler) send(ctx context.Context, chatID int64, r reply) (botapi.Message, bool) {
	b := h.bot()
	if b == nil || r.text == "" {
		return botapi.Message{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	p := botapi.SendMessageParams{ChatID: chatID, Text: r.text, LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}, ProtectContent: r.protect}
	switch {
	case r.force != nil:
		p.ReplyMarkup = r.force
	case r.markup != nil:
		p.ReplyMarkup = r.markup
	}
	m, err := b.SendMessage(ctx, p)
	if err != nil {
		h.o.Log.Debug("telegram: a chat reply was not sent", slog.String("error", err.Error()))
		return botapi.Message{}, false
	}
	return m, true
}

// show puts r on the message a button was tapped on, or sends it when there is none (or it can't be edited).
func (h *Handler) show(ctx context.Context, chatID int64, tapped *botapi.Message, r reply) {
	b := h.bot()
	if b != nil && tapped != nil && tapped.MessageID != 0 && r.force == nil {
		ectx, cancel := context.WithTimeout(ctx, apiTimeout)
		_, err := b.EditMessageText(ectx, botapi.EditMessageTextParams{ChatID: chatID, MessageID: tapped.MessageID, Text: r.text,
			LinkPreviewOptions: &botapi.LinkPreviewOptions{IsDisabled: true}, ReplyMarkup: r.markup})
		cancel()
		if err == nil {
			return
		}
	}
	h.send(ctx, chatID, r)
}

func (h *Handler) answer(ctx context.Context, id, text string) {
	b := h.bot()
	if b == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if err := b.AnswerCallbackQuery(ctx, botapi.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text}); err != nil {
		h.o.Log.Debug("telegram: a button tap was not answered", slog.String("error", err.Error()))
	}
}

// --- the API ---

// caller is one update's view of the API: the link's principal, built once per update.
type caller struct {
	h *Handler
	p *auth.Principal
}

// principal builds the link's principal; nil (and a short notice) when the link no longer acts.
func (h *Handler) principal(ctx context.Context, l updates.Link) *auth.Principal {
	if h.o.Principal == nil || h.o.API == nil {
		return nil
	}
	p, err := h.o.Principal(ctx, l.ID)
	if err != nil || p == nil || p.User == nil || p.User.ID != l.UserID {
		return nil
	}
	return p
}

type result struct {
	status int
	body   []byte
	etag   string
}

func (r result) ok() bool { return r.status >= 200 && r.status < 300 }

// problemDetail is the human text of an API problem, bounded and cleaned.
func (r result) problemDetail() string {
	var pr struct {
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(r.body, &pr)
	return clean(pr.Detail, detailRunes)
}

func (c caller) do(ctx context.Context, method, path string, body any, hdr map[string]string) result {
	var rd io.Reader
	ct := ""
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
		ct = "application/json"
	}
	return c.send(ctx, method, path, ct, rd, hdr)
}

// doRaw sends a body that is already encoded (a multipart upload).
func (c caller) doRaw(ctx context.Context, method, path, contentType string, body io.Reader) result {
	return c.send(ctx, method, path, contentType, body, nil)
}

func (c caller) send(ctx context.Context, method, path, contentType string, rd io.Reader, hdr map[string]string) result {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(api.WithInProcessPrincipal(ctx, c.p), method, "http://localhost"+api.Prefix+path, rd)
	if err != nil {
		return result{status: http.StatusInternalServerError}
	}
	req.RemoteAddr = "127.0.0.1:0"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	c.h.o.API.ServeHTTP(rec, req)
	return result{status: rec.Code, body: rec.Body.Bytes(), etag: rec.Header().Get("ETag")}
}

// get decodes a successful GET into out.
func (c caller) get(ctx context.Context, path string, out any) result {
	r := c.do(ctx, http.MethodGet, path, nil, nil)
	if r.ok() && out != nil {
		if err := json.Unmarshal(r.body, out); err != nil {
			r.status = http.StatusInternalServerError
		}
	}
	return r
}

// failText is the user-facing text for a failed API call. A hidden, missing or forbidden thing all read the same.
func failText(r result) string {
	switch r.status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusUnauthorized:
		return TextUnavailable
	case http.StatusConflict, http.StatusPreconditionFailed, http.StatusPreconditionRequired:
		return TextStale
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		if d := r.problemDetail(); d != "" {
			return fitUnits("Not accepted: "+d, callbackAnswerUnits)
		}
	}
	return TextFailed
}

// --- text helpers ---

// clean makes untrusted text safe for one line of plain text: control and format characters become spaces, runs of
// spaces collapse, and the result is cut to at most n runes.
func clean(s string, n int) string {
	var b strings.Builder
	space := false
	count := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			count++
			space = false
		}
		if count >= n {
			return strings.TrimSpace(b.String()) + "…"
		}
		b.WriteRune(r)
		count++
	}
	return b.String()
}

// units is the length of s in UTF-16 code units, the measure of the Bot API's limits.
func units(s string) int {
	n := 0
	for _, r := range s {
		n += runeUnits(r)
	}
	return n
}

func runeUnits(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

// fitUnits cuts s to at most n UTF-16 units without splitting a surrogate pair, marking the cut with "…".
func fitUnits(s string, n int) string {
	if units(s) <= n {
		return s
	}
	used := 1 // the ellipsis
	var b strings.Builder
	for _, r := range s {
		if used+runeUnits(r) > n {
			break
		}
		used += runeUnits(r)
		b.WriteRune(r)
	}
	return b.String() + "…"
}

// parts splits a body into at most maxBodyParts messages of at most budget UTF-16 units each, breaking at line ends
// where it can and never inside a surrogate pair; what doesn't fit is counted in dropped (runes).
func parts(body string, budget int) (out []string, dropped int) {
	rs := []rune(strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n"))
	for i := range rs {
		if rs[i] != '\n' && rs[i] != '\t' && (unicode.IsControl(rs[i]) || unicode.Is(unicode.Cf, rs[i])) {
			rs[i] = ' '
		}
	}
	for len(rs) > 0 {
		if len(out) == maxBodyParts {
			return out, len(rs)
		}
		n, u := 0, 0
		for n < len(rs) && u+runeUnits(rs[n]) <= budget {
			u += runeUnits(rs[n])
			n++
		}
		if n < len(rs) {
			for j := n; j > n/2; j-- {
				if rs[j-1] == '\n' {
					n = j
					break
				}
			}
		}
		if p := strings.TrimSpace(string(rs[:n])); p != "" {
			out = append(out, p)
		}
		rs = rs[n:]
	}
	return out, 0
}

func button(text, data string) botapi.InlineKeyboardButton {
	return botapi.InlineKeyboardButton{Text: text, CallbackData: data}
}

func markup(rows ...[]botapi.InlineKeyboardButton) *botapi.InlineKeyboardMarkup {
	out := rows[:0:0]
	for _, r := range rows {
		if len(r) > 0 {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &botapi.InlineKeyboardMarkup{InlineKeyboard: out}
}
