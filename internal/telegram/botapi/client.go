// Package botapi is SpaceMan's minimal Telegram Bot API client (ADR-0083, docs/contracts/telegram.md). It covers only
// the methods SpaceMan calls.
//
// The bot token is part of every request URL (`<base>/bot<token>/<method>`), so the client keeps it out of everything
// it returns or logs: errors name the method and Telegram's description, never a URL, and any text that could carry
// the token is redacted. The base URL comes from central's configuration file (`telegram.api_base_url`) and never from
// the API: an API-settable URL would let its caller receive the token (ADR-0083 decision 11).
package botapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL is Telegram's public Bot API endpoint.
const DefaultBaseURL = "https://api.telegram.org"

// MaxDownloadBytes is the largest file getFile can hand out (20 MB, API#getfile); DownloadFile reads no more.
const MaxDownloadBytes = 20 << 20

// maxResponseBytes bounds one JSON answer (100 updates with long texts fit many times over).
const maxResponseBytes = 16 << 20

// reToken is the documented token shape "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11" (API#authorizing-your-bot). Requiring
// it keeps a token from adding path segments, a query or a fragment to request URLs.
var reToken = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{1,128}$`)

// ValidToken reports whether token has the documented bot token shape.
func ValidToken(token string) bool { return reToken.MatchString(token) }

// ParseBaseURL accepts an absolute https URL without credentials, query or fragment, or an http URL on a loopback
// host (the fake server in tests, or a local Bot API server on the same machine). A trailing slash is dropped.
func ParseBaseURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("must be an absolute https URL, got %q", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, errors.New("must not contain credentials, a query or a fragment")
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, errors.New("must use https unless the host is loopback")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// Options configure a Client.
type Options struct {
	// BaseURL is central's `telegram.api_base_url` (default DefaultBaseURL). Never take it from an API request.
	BaseURL string
	// Token is the bot token. It must match the documented shape (ValidToken).
	Token string
	// HTTPClient sends requests (default: a client without a timeout; every call is bounded by its context). Redirects
	// are never followed, whatever the client says: a redirect could carry the token elsewhere.
	HTTPClient *http.Client
	// Logger receives one debug line per call with the method name, HTTP status and duration only (default: none).
	Logger *slog.Logger
}

// Client calls the Bot API. It is safe for concurrent use.
type Client struct {
	base   string // scheme://host[/path], no trailing slash
	token  string
	hc     *http.Client
	logger *slog.Logger
}

// New returns a client. Its errors never contain the token.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		o.BaseURL = DefaultBaseURL
	}
	u, err := ParseBaseURL(o.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("telegram: base URL %w", err)
	}
	if !ValidToken(o.Token) {
		return nil, errors.New("telegram: the bot token is malformed")
	}
	hc := &http.Client{}
	if o.HTTPClient != nil {
		c := *o.HTTPClient
		hc = &c
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: u.String(), token: o.Token, hc: hc, logger: o.Logger}, nil
}

// BotID is the numeric bot ID at the start of the token (public: it is the bot's user ID).
func (c *Client) BotID() string {
	id, _, _ := strings.Cut(c.token, ":")
	return id
}

// Call invokes method with params encoded as JSON and decodes the result into out (nil to ignore it). Prefer the typed
// methods; Call is for a method the typed set doesn't cover yet.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	var body io.Reader
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("telegram %s: encode parameters: %w", method, err)
		}
		body = bytes.NewReader(b)
	}
	return c.do(ctx, method, "application/json", body, out)
}

func (c *Client) methodURL(method string) string { return c.base + "/bot" + c.token + "/" + method }

// do sends one request and decodes Telegram's envelope.
func (c *Client) do(ctx context.Context, method, contentType string, body io.Reader, out any) error {
	if !reMethod.MatchString(method) {
		return fmt.Errorf("telegram: invalid method name %q", method)
	}
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.methodURL(method), body)
	if err != nil {
		return c.transportErr(method, err)
	}
	req.Header.Set("Content-Type", contentType)
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log(ctx, method, 0, start)
		return c.transportErr(method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.log(ctx, method, resp.StatusCode, start)
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return c.transportErr(method, err)
	}
	if len(raw) > maxResponseBytes {
		return &Error{Method: method, Code: resp.StatusCode, Description: "response too large"}
	}
	var env struct {
		OK          bool               `json:"ok"`
		Result      json.RawMessage    `json:"result"`
		ErrorCode   int                `json:"error_code"`
		Description string             `json:"description"`
		Parameters  ResponseParameters `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		// Not Telegram's envelope (a proxy error page, say): report the status, never the body.
		return &Error{Method: method, Code: resp.StatusCode, Description: "unexpected response: " + http.StatusText(resp.StatusCode)}
	}
	if !env.OK {
		code := env.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &Error{
			Method:          method,
			Code:            code,
			Description:     c.clean(env.Description),
			RetryAfter:      time.Duration(env.Parameters.RetryAfter) * time.Second,
			MigrateToChatID: env.Parameters.MigrateToChatID,
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("telegram %s: decode result: %s", method, c.clean(err.Error()))
	}
	return nil
}

// reMethod keeps method names to Telegram's identifier shape, so a name can't alter the URL.
var reMethod = regexp.MustCompile(`^[A-Za-z]{1,64}$`)

func (c *Client) log(ctx context.Context, method string, status int, start time.Time) {
	if c.logger == nil {
		return
	}
	c.logger.DebugContext(ctx, "telegram call", "method", method, "status", status, "duration", time.Since(start))
}

// clean redacts the token from text that came from the server or the network stack, and bounds its length.
func (c *Client) clean(s string) string {
	s = strings.ReplaceAll(s, c.token, "<redacted>")
	if id, secret, ok := strings.Cut(c.token, ":"); ok && id != "" {
		s = strings.ReplaceAll(s, secret, "<redacted>")
	}
	const maxLen = 300
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// transportErr turns a network-level failure into an error without the request URL (net/http's *url.Error prints the
// URL, and with it the token). Context cancellation and deadlines stay recognisable with errors.Is.
func (c *Client) transportErr(method string, err error) error {
	te := &TransportError{Method: method}
	if errors.Is(err, context.Canceled) {
		te.ctxErr = context.Canceled
	} else if errors.Is(err, context.DeadlineExceeded) {
		te.ctxErr = context.DeadlineExceeded
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		te.timeout = ue.Timeout()
		err = ue.Err
	}
	te.Msg = c.clean(err.Error())
	return te
}

// upload is one file part: its field name, file name and content.
type upload struct {
	field, name string
	r           io.Reader
}

// multipartBody streams a multipart/form-data request: fields as given, then the uploads. The returned content type
// carries the boundary.
func multipartBody(fields map[string]string, uploads []upload) (io.Reader, string) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for k, v := range fields {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			for _, f := range uploads {
				w, err := mw.CreateFormFile(f.field, f.name)
				if err != nil {
					return err
				}
				if _, err := io.Copy(w, f.r); err != nil {
					return err
				}
			}
			return mw.Close()
		}()
		_ = pw.CloseWithError(err)
	}()
	return pr, mw.FormDataContentType()
}

// callMultipart sends params as multipart/form-data with the uploads they reference (API#sending-files). Each top-level
// JSON field becomes a form value: strings as they are, everything else as JSON.
func (c *Client) callMultipart(ctx context.Context, method string, params any, uploads []*InputFile, out any) error {
	parts := make([]upload, len(uploads))
	for i, f := range uploads {
		f.attach = fmt.Sprintf("file%d", i)
		parts[i] = upload{field: f.attach, name: f.Name, r: f.Reader}
	}
	b, err := json.Marshal(params)
	for _, f := range uploads {
		f.attach = ""
	}
	if err != nil {
		return fmt.Errorf("telegram %s: encode parameters: %w", method, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		return fmt.Errorf("telegram %s: encode parameters: %w", method, err)
	}
	fields := make(map[string]string, len(top))
	for k, v := range top {
		var s string
		if len(v) > 0 && v[0] == '"' && json.Unmarshal(v, &s) == nil {
			fields[k] = s
			continue
		}
		fields[k] = string(v)
	}
	body, ct := multipartBody(fields, parts)
	return c.do(ctx, method, ct, body, out)
}

// DownloadFile copies the file at filePath (File.FilePath from GetFile) to w, reading at most MaxDownloadBytes. The
// file URL contains the token, so files are always fetched server-side (ADR-0083) and the URL is never returned.
func (c *Client) DownloadFile(ctx context.Context, filePath string, w io.Writer) (int64, error) {
	const method = "downloadFile"
	if !validFilePath(filePath) {
		return 0, errors.New("telegram downloadFile: invalid file path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+filePath, http.NoBody)
	if err != nil {
		return 0, c.transportErr(method, err)
	}
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		c.log(ctx, method, 0, start)
		return 0, c.transportErr(method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	c.log(ctx, method, resp.StatusCode, start)
	if resp.StatusCode != http.StatusOK {
		return 0, &Error{Method: method, Code: resp.StatusCode, Description: http.StatusText(resp.StatusCode)}
	}
	if resp.ContentLength > MaxDownloadBytes {
		return 0, &Error{Method: method, Code: resp.StatusCode, Description: "file is larger than 20 MB"}
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, MaxDownloadBytes+1))
	if err != nil {
		return n, c.transportErr(method, err)
	}
	if n > MaxDownloadBytes {
		return n, &Error{Method: method, Code: resp.StatusCode, Description: "file is larger than 20 MB"}
	}
	return n, nil
}

// reFilePath is the shape of getFile paths ("documents/file_1.pdf"); anything else is refused rather than escaped.
var reFilePath = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)*$`)

func validFilePath(p string) bool {
	if len(p) > 512 || !reFilePath.MatchString(p) {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}
