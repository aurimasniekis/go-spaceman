package tgtest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// dispatch runs one method. Descriptions copy Telegram's where the contract records them; the others are the fake's
// own and start with the same "Bad Request: " prefix, so callers must not depend on their wording.
func (s *Server) dispatch(r *http.Request, method string, p params, files map[string][]byte) (any, *apiError) {
	s.mu.Lock()
	if q := s.fail[method]; len(q) > 0 {
		f := q[0]
		s.fail[method] = q[1:]
		s.mu.Unlock()
		return nil, &apiError{code: f.code, desc: f.desc, retryAfter: f.retryAfter}
	}
	s.mu.Unlock()
	if method == "getUpdates" {
		return s.getUpdates(r, p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case "getMe":
		return s.opts.Bot, nil
	case "setWebhook":
		return s.setWebhookLocked(p)
	case "deleteWebhook":
		s.webhook = botapi.WebhookInfo{}
		s.secret = ""
		if p.bool("drop_pending_updates") {
			s.updates = nil
		}
		return true, nil
	case "getWebhookInfo":
		w := s.webhook
		if w.URL != "" {
			w.PendingUpdateCount = len(s.updates)
		}
		return w, nil
	case "sendMessage":
		return s.sendMessageLocked(p)
	case "editMessageText":
		return s.editMessageTextLocked(p)
	case "editMessageReplyMarkup":
		return s.editReplyMarkupLocked(p)
	case "deleteMessage":
		return s.deleteMessageLocked(p)
	case "answerCallbackQuery":
		return s.answerCallbackLocked(p)
	case "setMyCommands":
		var cmds []botapi.BotCommand
		if err := p.json("commands", &cmds); err != nil {
			return nil, badRequest("can't parse commands JSON object")
		}
		if len(cmds) > MaxCommands {
			return nil, badRequest("too many commands")
		}
		s.commands = cmds
		return true, nil
	case "setChatMenuButton":
		var mb botapi.MenuButton
		if err := p.json("menu_button", &mb); err != nil {
			return nil, badRequest("can't parse menu button JSON object")
		}
		if mb.Type == botapi.MenuButtonWebApp && (mb.WebApp == nil || !strings.HasPrefix(mb.WebApp.URL, "https://")) {
			return nil, badRequest("web app URL must be https")
		}
		s.menu = &mb
		return true, nil
	case "getFile":
		f, ok := s.files[p.str("file_id")]
		if !ok {
			return nil, badRequest("invalid file_id")
		}
		if f.size > MaxGetFileBytes {
			return nil, badRequest("file is too big")
		}
		return botapi.File{FileID: f.id, FileUniqueID: f.uniqueID, FileSize: f.size, FilePath: f.path}, nil
	case "sendDocument":
		return s.sendFileLocked(p, files, botapi.MediaDocument)
	case "sendPhoto":
		return s.sendFileLocked(p, files, botapi.MediaPhoto)
	case "sendMediaGroup":
		return s.sendMediaGroupLocked(p, files)
	default:
		return nil, &apiError{code: http.StatusNotFound, desc: "Not Found"}
	}
}

// reSecret is setWebhook's secret_token alphabet (API#setwebhook).
var reSecret = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func (s *Server) setWebhookLocked(p params) (any, *apiError) {
	raw := p.str("url")
	if raw == "" {
		s.webhook = botapi.WebhookInfo{}
		s.secret = ""
		return true, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, badRequest("bad webhook: An HTTPS URL must be provided for webhook")
	}
	switch u.Port() {
	case "", "443", "80", "88", "8443":
	default:
		return nil, badRequest("bad webhook: Webhook can be set up only on ports 80, 88, 443 or 8443")
	}
	secret, hasSecret := p["secret_token"]
	if hasSecret && !reSecret.MatchString(secret) {
		return nil, badRequest("bad webhook: secret token contains unallowed characters")
	}
	maxConn := 40
	if n, ok := p.int64("max_connections"); ok {
		if n < 1 || n > 100 {
			return nil, badRequest("bad webhook: max_connections must be within 1..100")
		}
		maxConn = int(n)
	}
	var allowed []string
	if err := p.json("allowed_updates", &allowed); err != nil {
		return nil, badRequest("can't parse allowed_updates")
	}
	if p.bool("drop_pending_updates") {
		s.updates = nil
	}
	s.webhook = botapi.WebhookInfo{URL: raw, MaxConnections: maxConn, AllowedUpdates: allowed, IPAddress: p.str("ip_address")}
	s.secret = secret
	return true, nil
}

// targetLocked resolves chat_id for a send, edit or delete: the chat must be given and not have blocked the bot, and
// the send must fit the rate limits.
func (s *Server) targetLocked(p params) (botapi.Chat, *apiError) {
	id, ok := p.int64("chat_id")
	if !ok || id == 0 {
		return botapi.Chat{}, badRequest("chat not found")
	}
	if s.blocked[id] {
		return botapi.Chat{}, &apiError{code: http.StatusForbidden, desc: "Forbidden: bot was blocked by the user"}
	}
	chat := s.chatLocked(id)
	if e := s.rateLimitLocked(chat); e != nil {
		return botapi.Chat{}, e
	}
	return chat, nil
}

func (s *Server) newMessageLocked(chat botapi.Chat, p params) *botapi.Message {
	s.nextMsg[chat.ID]++
	bot := s.opts.Bot
	m := &botapi.Message{MessageID: s.nextMsg[chat.ID], From: &bot, Chat: chat, Date: s.clk.Now().Unix()}
	if n, ok := p.int64("message_thread_id"); ok {
		m.MessageThreadID = int(n)
	}
	if s.messages[chat.ID] == nil {
		s.messages[chat.ID] = map[int]*botapi.Message{}
	}
	s.messages[chat.ID][m.MessageID] = m
	return m
}

func (s *Server) replyLocked(chat botapi.Chat, p params) (*botapi.Message, *apiError) {
	var rp botapi.ReplyParameters
	if err := p.json("reply_parameters", &rp); err != nil {
		return nil, badRequest("can't parse reply parameters JSON object")
	}
	if rp.MessageID == 0 {
		return nil, nil
	}
	m, ok := s.messages[chat.ID][rp.MessageID]
	if !ok {
		return nil, badRequest("message to be replied not found")
	}
	cp := *m
	cp.ReplyToMessage = nil
	return &cp, nil
}

func (s *Server) sendMessageLocked(p params) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	text := p.str("text")
	switch n := utf16Len(text); {
	case n == 0 || strings.TrimSpace(text) == "":
		return nil, badRequest("message text is empty")
	case n > MaxTextLen:
		return nil, badRequest("message is too long")
	}
	ents, e := entities(p, "entities", text)
	if e != nil {
		return nil, e
	}
	kb, e := markup(p, chat)
	if e != nil {
		return nil, e
	}
	reply, e := s.replyLocked(chat, p)
	if e != nil {
		return nil, e
	}
	m := s.newMessageLocked(chat, p)
	m.Text, m.Entities, m.ReplyMarkup, m.ReplyToMessage = text, ents, kb, reply
	return m, nil
}

func (s *Server) existingLocked(chat botapi.Chat, p params, what string) (*botapi.Message, *apiError) {
	id, _ := p.int64("message_id")
	m, ok := s.messages[chat.ID][int(id)]
	if !ok {
		return nil, badRequest("message to %s not found", what)
	}
	return m, nil
}

const notModified = "message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"

func (s *Server) editMessageTextLocked(p params) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	m, e := s.existingLocked(chat, p, "edit")
	if e != nil {
		return nil, e
	}
	if m.Text == "" {
		return nil, badRequest("there is no text in the message to edit")
	}
	text := p.str("text")
	switch n := utf16Len(text); {
	case n == 0 || strings.TrimSpace(text) == "":
		return nil, badRequest("message text is empty")
	case n > MaxTextLen:
		return nil, badRequest("message is too long")
	}
	ents, e := entities(p, "entities", text)
	if e != nil {
		return nil, e
	}
	kb, e := markup(p, chat)
	if e != nil {
		return nil, e
	}
	if text == m.Text && sameJSON(ents, m.Entities) && sameJSON(kb, m.ReplyMarkup) {
		return nil, badRequest(notModified)
	}
	m.Text, m.Entities, m.ReplyMarkup, m.EditDate = text, ents, kb, s.clk.Now().Unix()
	return m, nil
}

func (s *Server) editReplyMarkupLocked(p params) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	m, e := s.existingLocked(chat, p, "edit")
	if e != nil {
		return nil, e
	}
	kb, e := markup(p, chat)
	if e != nil {
		return nil, e
	}
	if sameJSON(kb, m.ReplyMarkup) {
		return nil, badRequest(notModified)
	}
	m.ReplyMarkup, m.EditDate = kb, s.clk.Now().Unix()
	return m, nil
}

func (s *Server) deleteMessageLocked(p params) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	m, e := s.existingLocked(chat, p, "delete")
	if e != nil {
		return nil, e
	}
	if s.clk.Now().Sub(time.Unix(m.Date, 0)) > 48*time.Hour {
		return nil, badRequest("message can't be deleted")
	}
	delete(s.messages[chat.ID], m.MessageID)
	return true, nil
}

func (s *Server) answerCallbackLocked(p params) (any, *apiError) {
	id := p.str("callback_query_id")
	q, ok := s.callbacks[id]
	if !ok || q.answered {
		return nil, badRequest("query is too old and response timeout expired or query ID is invalid")
	}
	text := p.str("text")
	if utf16Len(text) > MaxCallbackAnswer {
		return nil, badRequest("callback answer text is too long")
	}
	q.answered = true
	s.answers = append(s.answers, botapi.AnswerCallbackQueryParams{
		CallbackQueryID: id, Text: text, ShowAlert: p.bool("show_alert"), URL: p.str("url"),
	})
	return true, nil
}

// inputFile resolves a file parameter: "attach://<name>" names a multipart part, a part named after the parameter is
// a direct upload, anything else must be a file_id the fake issued. Sending by URL isn't simulated.
func (s *Server) inputFileLocked(val, param string, files map[string][]byte) (content []byte, existing *storedFile, e *apiError) {
	if name, ok := strings.CutPrefix(val, "attach://"); ok {
		b, ok := files[name]
		if !ok {
			return nil, nil, badRequest("can't find attached file %q", name)
		}
		return b, nil, nil
	}
	if val == "" {
		if b, ok := files[param]; ok {
			return b, nil, nil
		}
		return nil, nil, badRequest("there is no %s in the request", param)
	}
	f, ok := s.files[val]
	if !ok {
		return nil, nil, badRequest("wrong file identifier/HTTP URL specified")
	}
	return nil, f, nil
}

func (s *Server) mediaLocked(kind, val, param string, files map[string][]byte, m *botapi.Message) *apiError {
	content, f, e := s.inputFileLocked(val, param, files)
	if e != nil {
		return e
	}
	if f == nil {
		limit := int64(MaxUploadBytes)
		if kind == botapi.MediaPhoto {
			limit = MaxPhotoUpload
		}
		if int64(len(content)) > limit {
			return &apiError{code: http.StatusRequestEntityTooLarge, desc: "Request Entity Too Large"}
		}
		f = s.storeFileLocked("upload", content, int64(len(content)))
	}
	switch kind {
	case botapi.MediaPhoto:
		m.Photo = []botapi.PhotoSize{{FileID: f.id, FileUniqueID: f.uniqueID, Width: 1280, Height: 720, FileSize: f.size}}
	default:
		m.Document = &botapi.Document{FileID: f.id, FileUniqueID: f.uniqueID, FileSize: f.size}
	}
	return nil
}

func caption(p params, key, entKey string) (string, []botapi.MessageEntity, *apiError) {
	c := p.str(key)
	if utf16Len(c) > MaxCaptionLen {
		return "", nil, badRequest("message caption is too long")
	}
	ents, e := entities(p, entKey, c)
	return c, ents, e
}

func (s *Server) sendFileLocked(p params, files map[string][]byte, kind string) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	capText, capEnts, e := caption(p, "caption", "caption_entities")
	if e != nil {
		return nil, e
	}
	kb, e := markup(p, chat)
	if e != nil {
		return nil, e
	}
	reply, e := s.replyLocked(chat, p)
	if e != nil {
		return nil, e
	}
	var m botapi.Message
	if e := s.mediaLocked(kind, p.str(kind), kind, files, &m); e != nil {
		return nil, e
	}
	stored := s.newMessageLocked(chat, p)
	stored.Photo, stored.Document = m.Photo, m.Document
	stored.Caption, stored.CaptionEntities, stored.ReplyMarkup, stored.ReplyToMessage = capText, capEnts, kb, reply
	return stored, nil
}

// sendMediaGroupLocked counts an album as one send against the rate limits: Telegram doesn't document how albums count,
// and counting each item would make a 10-item album impossible under a burst of 3.
func (s *Server) sendMediaGroupLocked(p params, files map[string][]byte) (any, *apiError) {
	chat, e := s.targetLocked(p)
	if e != nil {
		return nil, e
	}
	var media []struct {
		Type            string                 `json:"type"`
		Media           string                 `json:"media"`
		Caption         string                 `json:"caption"`
		CaptionEntities []botapi.MessageEntity `json:"caption_entities"`
	}
	if err := p.json("media", &media); err != nil {
		return nil, badRequest("can't parse media JSON object")
	}
	if len(media) < MinMediaGroup || len(media) > MaxMediaGroup {
		return nil, badRequest("media group must have %d-%d items", MinMediaGroup, MaxMediaGroup)
	}
	docs := 0
	for _, it := range media {
		switch it.Type {
		case botapi.MediaDocument:
			docs++
		case botapi.MediaPhoto, "video", "audio":
		default:
			return nil, badRequest("unsupported media type %q", it.Type)
		}
	}
	if docs != 0 && docs != len(media) {
		return nil, badRequest("documents can be grouped only with documents")
	}
	reply, e := s.replyLocked(chat, p)
	if e != nil {
		return nil, e
	}
	built := make([]botapi.Message, len(media))
	for i, it := range media {
		if utf16Len(it.Caption) > MaxCaptionLen {
			return nil, badRequest("message caption is too long")
		}
		if e := checkEntities(it.Caption, it.CaptionEntities); e != nil {
			return nil, e
		}
		kind := it.Type
		if kind != botapi.MediaPhoto {
			kind = botapi.MediaDocument
		}
		if e := s.mediaLocked(kind, it.Media, "", files, &built[i]); e != nil {
			return nil, e
		}
		built[i].Caption, built[i].CaptionEntities = it.Caption, it.CaptionEntities
	}
	s.nextGroup++
	group := "MG" + strconv.Itoa(s.nextGroup)
	out := make([]botapi.Message, len(media))
	for i := range built {
		m := s.newMessageLocked(chat, p)
		m.Photo, m.Document, m.Caption, m.CaptionEntities = built[i].Photo, built[i].Document, built[i].Caption, built[i].CaptionEntities
		m.MediaGroupID, m.ReplyToMessage = group, reply
		out[i] = *m
	}
	return out, nil
}

// entities parses and checks an entities parameter against text.
func entities(p params, key, text string) ([]botapi.MessageEntity, *apiError) {
	var ents []botapi.MessageEntity
	if err := p.json(key, &ents); err != nil {
		return nil, badRequest("can't parse entities JSON object")
	}
	if e := checkEntities(text, ents); e != nil {
		return nil, e
	}
	return ents, nil
}

// checkEntities enforces what the contract records: offsets and lengths in UTF-16 units inside the text, overlapping
// entities nest, pre and code contain nothing, blockquotes don't nest.
func checkEntities(text string, ents []botapi.MessageEntity) *apiError {
	n := utf16Len(text)
	sorted := append([]botapi.MessageEntity(nil), ents...)
	for _, e := range sorted {
		if e.Offset < 0 || e.Length < 1 || e.Offset+e.Length > n {
			return badRequest("can't parse entities: entity %s at %d+%d is outside the text (%d UTF-16 units)", e.Type, e.Offset, e.Length, n)
		}
		if e.Type == "text_link" && e.URL == "" {
			return badRequest("can't parse entities: text_link without url")
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Offset != sorted[j].Offset {
			return sorted[i].Offset < sorted[j].Offset
		}
		return sorted[i].Length > sorted[j].Length
	})
	var open []botapi.MessageEntity
	for _, e := range sorted {
		for len(open) > 0 && open[len(open)-1].Offset+open[len(open)-1].Length <= e.Offset {
			open = open[:len(open)-1]
		}
		for _, o := range open {
			if e.Offset+e.Length > o.Offset+o.Length {
				return badRequest("can't parse entities: entities %s and %s overlap without nesting", o.Type, e.Type)
			}
			if o.Type == "pre" || o.Type == "code" {
				return badRequest("can't parse entities: %s can't contain other entities", o.Type)
			}
			if isQuote(o.Type) && isQuote(e.Type) {
				return badRequest("can't parse entities: blockquotes can't be nested")
			}
		}
		open = append(open, e)
	}
	return nil
}

func isQuote(t string) bool { return t == "blockquote" || t == "expandable_blockquote" }

// markup parses and checks reply_markup. Only an inline keyboard is kept on the stored message.
func markup(p params, chat botapi.Chat) (*botapi.InlineKeyboardMarkup, *apiError) {
	raw, ok := p["reply_markup"]
	if !ok || raw == "" || raw == "null" {
		return nil, nil
	}
	var m struct {
		InlineKeyboard [][]map[string]json.RawMessage `json:"inline_keyboard"`
		ForceReply     bool                           `json:"force_reply"`
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, badRequest("can't parse reply keyboard markup JSON object")
	}
	if m.InlineKeyboard == nil {
		if m.ForceReply {
			return nil, nil
		}
		return nil, badRequest("unsupported reply markup")
	}
	for _, row := range m.InlineKeyboard {
		for _, b := range row {
			if e := checkButton(b, chat); e != nil {
				return nil, e
			}
		}
	}
	var kb botapi.InlineKeyboardMarkup
	if err := json.Unmarshal([]byte(raw), &kb); err != nil {
		return nil, badRequest("can't parse inline keyboard button")
	}
	return &kb, nil
}

// buttonActions are the inline button fields of which exactly one must be set (API#inlinekeyboardbutton).
var buttonActions = []string{"url", "callback_data", "web_app", "login_url", "switch_inline_query",
	"switch_inline_query_current_chat", "switch_inline_query_chosen_chat", "copy_text", "callback_game", "pay"}

func checkButton(b map[string]json.RawMessage, chat botapi.Chat) *apiError {
	var text string
	if err := json.Unmarshal(b["text"], &text); err != nil || text == "" {
		return badRequest("can't parse inline keyboard button: text must be non-empty")
	}
	actions := 0
	for _, k := range buttonActions {
		if _, ok := b[k]; ok {
			actions++
		}
	}
	if actions != 1 {
		return badRequest("can't parse inline keyboard button: exactly one action field is required")
	}
	if raw, ok := b["callback_data"]; ok {
		var data string
		if err := json.Unmarshal(raw, &data); err != nil || len(data) < 1 || len(data) > MaxCallbackData {
			return badRequest("BUTTON_DATA_INVALID")
		}
	}
	if _, ok := b["web_app"]; ok && chat.Type != botapi.ChatPrivate {
		return badRequest("BUTTON_TYPE_INVALID")
	}
	return nil
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(normNull(x), normNull(y))
}

// normNull treats empty slices, nil slices and nil pointers alike.
func normNull(b []byte) []byte {
	if string(b) == "[]" {
		return []byte("null")
	}
	return b
}
