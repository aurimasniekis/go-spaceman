// Package tgtest is a fake Telegram Bot API server for tests (ADR-0083, plan §5.3). It is built from
// docs/contracts/telegram.md, not from observation of the real API, so passing against it is "tested (simulated)"
// evidence only.
//
// The fake:
//   - serves `/bot<token>/<method>` for the methods SpaceMan uses and `/file/bot<token>/<path>` downloads, accepting
//     parameters as a query string, a URL-encoded form, JSON or multipart/form-data (API#making-requests);
//   - records every call, accepted or refused;
//   - enforces the documented limits SpaceMan depends on: 1–4096 characters of text, 1024 of caption, 1–64 bytes of
//     callback_data, 2–10 items in a media group (documents only with documents), 20 MB for getFile, `web_app` buttons
//     only in private chats, entity offsets inside the text;
//   - answers 429 with parameters.retry_after when a sender exceeds about 1 message/s per chat (short bursts
//     allowed), 20/min per group or about 30/s overall (FAQ), so a sender that is too fast fails its tests;
//   - delivers updates by webhook POST (with or without the right X-Telegram-Bot-Api-Secret-Token) and by getUpdates.
//
// Lengths are counted in UTF-16 code units, the stricter of the two plausible readings of "characters" (the docs don't
// say; an emoji counts as two).
package tgtest

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Documented limits (docs/contracts/telegram.md).
const (
	MaxTextLen         = 4096
	MaxCaptionLen      = 1024
	MaxCallbackData    = 64
	MinMediaGroup      = 2
	MaxMediaGroup      = 10
	MaxGetFileBytes    = 20 << 20
	MaxUploadBytes     = 50 << 20
	MaxPhotoUpload     = 10 << 20
	MaxCallbackAnswer  = 200
	MaxCommands        = 100
	MaxUpdatesPerPoll  = 100
	maxRequestBodySize = MaxUploadBytes + 1<<20
)

// Limits are the fake's rate limits. The zero value of a field means its default.
type Limits struct {
	// ChatPerSecond and ChatBurst bound sends to one private chat (default 1/s with a burst of 3).
	ChatPerSecond float64
	ChatBurst     int
	// GroupPerMinute bounds sends to one group or supergroup (default 20/min, burst 20).
	GroupPerMinute int
	// GlobalPerSecond bounds all sends together (default 30/s, burst 30).
	GlobalPerSecond float64
	// Disabled turns rate limiting off.
	Disabled bool
}

func (l Limits) withDefaults() Limits {
	if l.ChatPerSecond == 0 {
		l.ChatPerSecond = 1
	}
	if l.ChatBurst == 0 {
		l.ChatBurst = 3
	}
	if l.GroupPerMinute == 0 {
		l.GroupPerMinute = 20
	}
	if l.GlobalPerSecond == 0 {
		l.GlobalPerSecond = 30
	}
	return l
}

// Options configure the fake.
type Options struct {
	// Clock drives rate limiting and message dates (default: the system clock). A clock.Fake makes 429 tests exact.
	Clock clock.Clock
	// Limits are the rate limits.
	Limits Limits
	// Bot is the bot getMe returns (default: ID from the token, username "spaceman_test_bot").
	Bot botapi.User
	// WebhookTarget, when set, receives webhook deliveries instead of the URL registered with setWebhook. setWebhook
	// still checks that URL as Telegram does (https, port 443/80/88/8443), so a test can register a realistic URL and
	// deliver to its httptest server.
	WebhookTarget string
}

// Call is one recorded request.
type Call struct {
	Method string
	// Params holds each parameter as sent: strings as they are, other JSON values as JSON text.
	Params map[string]string
	// Files holds the multipart uploads by field name.
	Files map[string][]byte
	// Status is the HTTP status the fake answered.
	Status int
	At     time.Time
}

type storedFile struct {
	id, uniqueID, path string
	size               int64
	content            []byte
}

// Server is a running fake.
type Server struct {
	srv   *httptest.Server
	token string
	opts  Options
	clk   clock.Clock

	mu       sync.Mutex
	calls    []Call
	chats    map[int64]botapi.Chat
	blocked  map[int64]bool
	messages map[int64]map[int]*botapi.Message
	nextMsg  map[int64]int
	files    map[string]*storedFile // by file_id
	paths    map[string]*storedFile // by file_path
	fail     map[string][]failure

	chatBuckets  map[int64]*bucket
	globalBucket *bucket

	webhook   botapi.WebhookInfo
	secret    string
	updates   []botapi.Update // pending for getUpdates
	nextUpd   int64
	updateCh  chan struct{} // closed and replaced when an update arrives
	commands  []botapi.BotCommand
	menu      *botapi.MenuButton
	answers   []botapi.AnswerCallbackQueryParams
	nextFile  int
	nextGroup int
	callbacks map[string]*callbackState
}

type callbackState struct{ answered bool }

type failure struct {
	code       int
	desc       string
	retryAfter int
}

// New starts a fake with a random test token; it stops at test cleanup.
func New(t testing.TB, o Options) *Server {
	t.Helper()
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	o.Limits = o.Limits.withDefaults()
	s := &Server{
		opts: o, clk: o.Clock,
		token:     randomToken(),
		chats:     map[int64]botapi.Chat{},
		blocked:   map[int64]bool{},
		messages:  map[int64]map[int]*botapi.Message{},
		nextMsg:   map[int64]int{},
		files:     map[string]*storedFile{},
		paths:     map[string]*storedFile{},
		fail:      map[string][]failure{},
		callbacks: map[string]*callbackState{},

		chatBuckets: map[int64]*bucket{},
		nextUpd:     1,
		updateCh:    make(chan struct{}),
	}
	s.globalBucket = newBucket(o.Limits.GlobalPerSecond, float64(int(o.Limits.GlobalPerSecond)), s.clk.Now())
	if s.opts.Bot.ID == 0 {
		id, _, _ := strings.Cut(s.token, ":")
		s.opts.Bot.ID, _ = strconv.ParseInt(id, 10, 64)
	}
	if s.opts.Bot.FirstName == "" {
		s.opts.Bot.FirstName = "SpaceMan Test Bot"
	}
	if s.opts.Bot.Username == "" {
		s.opts.Bot.Username = "spaceman_test_bot"
	}
	s.opts.Bot.IsBot = true
	s.srv = httptest.NewUnstartedServer(http.HandlerFunc(s.serveHTTP))
	s.srv.Config.ErrorLog = slog.NewLogLogger(slog.DiscardHandler, slog.LevelError)
	s.srv.Start()
	t.Cleanup(s.srv.Close)
	return s
}

// randomToken makes a token of the documented shape, generated at test time (no secret fixtures).
func randomToken() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	id := 1_000_000_000 + binary.BigEndian.Uint32(b[:4])%1_000_000_000
	return fmt.Sprintf("%d:%s", id, base64.RawURLEncoding.EncodeToString(b[4:]))
}

// URL is the base URL to configure as telegram.api_base_url.
func (s *Server) URL() string { return s.srv.URL }

// Token is the bot token the fake accepts.
func (s *Server) Token() string { return s.token }

// Bot is the bot getMe returns.
func (s *Server) Bot() botapi.User { return s.opts.Bot }

// Close stops the server early (it also stops at test cleanup).
func (s *Server) Close() { s.srv.Close() }

// SetChat registers a chat's type. Unregistered chats are private when their ID is positive and supergroups when it
// is negative, as Telegram numbers them.
func (s *Server) SetChat(c botapi.Chat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chats[c.ID] = c
}

// Block makes sends to chatID fail with 403 "bot was blocked by the user" until Unblock.
func (s *Server) Block(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blocked[chatID] = true
}

// Unblock undoes Block.
func (s *Server) Unblock(chatID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.blocked, chatID)
}

// FailNext makes the next call of method answer with code and description (and retry_after seconds when positive),
// before any other check. Calls queue up per method.
func (s *Server) FailNext(method string, code int, description string, retryAfter int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail[method] = append(s.fail[method], failure{code: code, desc: description, retryAfter: retryAfter})
}

// Calls returns the recorded calls (including those refused for a wrong token), all of them or only those of the
// given methods.
func (s *Server) Calls(methods ...string) []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Call
	for _, c := range s.calls {
		if len(methods) == 0 || contains(methods, c.Method) {
			out = append(out, c)
		}
	}
	return out
}

// Messages returns the messages currently in chatID (sent and not deleted, with edits applied), oldest first.
func (s *Server) Messages(chatID int64) []botapi.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int, 0, len(s.messages[chatID]))
	for id := range s.messages[chatID] {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	out := make([]botapi.Message, 0, len(ids))
	for _, id := range ids {
		out = append(out, *s.messages[chatID][id])
	}
	return out
}

// Commands returns the commands last set with setMyCommands.
func (s *Server) Commands() []botapi.BotCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]botapi.BotCommand(nil), s.commands...)
}

// MenuButton returns the menu button last set with setChatMenuButton.
func (s *Server) MenuButton() *botapi.MenuButton {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.menu
}

// CallbackAnswers returns the answerCallbackQuery calls the fake accepted.
func (s *Server) CallbackAnswers() []botapi.AnswerCallbackQueryParams {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]botapi.AnswerCallbackQueryParams(nil), s.answers...)
}

// AddFile stores a file a user "sent" and returns its file_id for getFile. With content nil the file has size bytes
// but no content (to test the 20 MB getFile limit without allocating it).
func (s *Server) AddFile(name string, content []byte, size int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if content != nil {
		size = int64(len(content))
	}
	return s.storeFileLocked(name, content, size).id
}

func (s *Server) storeFileLocked(name string, content []byte, size int64) *storedFile {
	s.nextFile++
	n := s.nextFile
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 && validExt(name[i+1:]) {
		ext = name[i:]
	}
	f := &storedFile{
		id:       fmt.Sprintf("FILE%06d", n),
		uniqueID: fmt.Sprintf("U%06d", n),
		path:     fmt.Sprintf("documents/file_%d%s", n, ext),
		size:     size,
		content:  content,
	}
	s.files[f.id] = f
	s.paths[f.path] = f
	return f
}

func validExt(e string) bool {
	if len(e) > 8 {
		return false
	}
	for _, r := range e {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// FileContent returns an uploaded or added file by file_id.
func (s *Server) FileContent(fileID string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.files[fileID]
	if !ok {
		return nil, false
	}
	return f.content, true
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// apiError is a refusal in Telegram's envelope.
type apiError struct {
	code       int
	desc       string
	retryAfter int
}

func badRequest(format string, args ...any) *apiError {
	return &apiError{code: http.StatusBadRequest, desc: "Bad Request: " + fmt.Sprintf(format, args...)}
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case strings.HasPrefix(p, "/file/bot"):
		s.serveFile(w, strings.TrimPrefix(p, "/file/bot"))
	case strings.HasPrefix(p, "/bot"):
		token, method, _ := strings.Cut(strings.TrimPrefix(p, "/bot"), "/")
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			s.record(Call{Method: method, Status: http.StatusUnauthorized})
			writeEnvelope(w, nil, &apiError{code: http.StatusUnauthorized, desc: "Unauthorized"})
			return
		}
		s.serveMethod(w, r, method)
	default:
		writeEnvelope(w, nil, &apiError{code: http.StatusNotFound, desc: "Not Found"})
	}
}

func (s *Server) serveFile(w http.ResponseWriter, rest string) {
	token, path, _ := strings.Cut(rest, "/")
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	f, ok := s.paths[path]
	s.mu.Unlock()
	if !ok || f.content == nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.content)))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(f.content)
}

// params is a request's parameters, normalised as Call.Params is.
type params map[string]string

func (p params) str(k string) string { return p[k] }

func (p params) int64(k string) (int64, bool) {
	v, ok := p[k]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

func (p params) bool(k string) bool { return p[k] == "true" }

func (p params) json(k string, v any) error {
	raw, ok := p[k]
	if !ok {
		return nil
	}
	return json.Unmarshal([]byte(raw), v)
}

func parseRequest(r *http.Request) (params, map[string][]byte, *apiError) {
	out := params{}
	for k, v := range r.URL.Query() {
		out[k] = v[0]
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxRequestBodySize)
	ct, ctParams, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	files := map[string][]byte{}
	switch ct {
	case "", "application/json":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, nil, &apiError{code: http.StatusRequestEntityTooLarge, desc: "Request Entity Too Large"}
		}
		if len(bytes.TrimSpace(body)) == 0 {
			return out, files, nil
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, nil, badRequest("can't parse request JSON object")
		}
		for k, v := range m {
			var str string
			if len(v) > 0 && v[0] == '"' && json.Unmarshal(v, &str) == nil {
				out[k] = str
			} else {
				out[k] = string(v)
			}
		}
	case "application/x-www-form-urlencoded":
		if err := r.ParseForm(); err != nil {
			return nil, nil, badRequest("can't parse form")
		}
		for k, v := range r.PostForm {
			out[k] = v[0]
		}
	case "multipart/form-data":
		if ctParams["boundary"] == "" {
			return nil, nil, badRequest("no multipart boundary")
		}
		// The body is already bounded by MaxBytesReader above.
		if err := r.ParseMultipartForm(1 << 20); err != nil { //nolint:gosec // G120: bounded by MaxBytesReader
			return nil, nil, &apiError{code: http.StatusRequestEntityTooLarge, desc: "Request Entity Too Large"}
		}
		for k, v := range r.MultipartForm.Value {
			out[k] = v[0]
		}
		for k, fhs := range r.MultipartForm.File {
			f, err := fhs[0].Open()
			if err != nil {
				return nil, nil, badRequest("can't read file %s", k)
			}
			b, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil {
				return nil, nil, badRequest("can't read file %s", k)
			}
			files[k] = b
		}
	default:
		return nil, nil, badRequest("unsupported content type")
	}
	return out, files, nil
}

func (s *Server) serveMethod(w http.ResponseWriter, r *http.Request, method string) {
	p, files, perr := parseRequest(r)
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	var result any
	var aerr *apiError
	if perr != nil {
		aerr = perr
	} else {
		result, aerr = s.dispatch(r, method, p, files)
	}
	status := http.StatusOK
	if aerr != nil {
		status = aerr.code
	}
	s.record(Call{Method: method, Params: p, Files: files, Status: status})
	writeEnvelope(w, result, aerr)
}

func (s *Server) record(c Call) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.At = s.clk.Now()
	s.calls = append(s.calls, c)
}

func writeEnvelope(w http.ResponseWriter, result any, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	if e != nil {
		body := map[string]any{"ok": false, "error_code": e.code, "description": e.desc}
		if e.retryAfter > 0 {
			body["parameters"] = map[string]any{"retry_after": e.retryAfter}
		}
		w.WriteHeader(e.code)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

// chatLocked returns the chat for id: registered, or derived from the sign of the ID.
func (s *Server) chatLocked(id int64) botapi.Chat {
	if c, ok := s.chats[id]; ok {
		return c
	}
	if id > 0 {
		return botapi.Chat{ID: id, Type: botapi.ChatPrivate, FirstName: "User " + strconv.FormatInt(id, 10)}
	}
	return botapi.Chat{ID: id, Type: botapi.ChatSupergroup, Title: "Group " + strconv.FormatInt(-id, 10)}
}
