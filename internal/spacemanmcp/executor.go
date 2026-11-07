package spacemanmcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// DefaultMaxResultBytes caps a tool result when the server sets no limit (the gateway's default).
const DefaultMaxResultBytes = 256 << 10

// Executor runs tool calls through the API's in-process handler (api.Deps.InProcessHandler): the operation's rule,
// validation, idempotency and audit apply exactly as over HTTP, with the caller's principal.
type Executor struct {
	// Handler is api.Deps.InProcessHandler().
	Handler http.Handler
	// MaxResultBytes caps the response body a result carries (DefaultMaxResultBytes when zero).
	MaxResultBytes int
}

// Call is one tool call.
type Call struct {
	Principal *auth.Principal
	Tool      *Tool
	// Args are the tool's JSON arguments (an object; empty means none).
	Args json.RawMessage
	// DefaultSpaceID fills a missing space_id path argument (a server whose grants name exactly one space).
	DefaultSpaceID string
}

// Result is the JSON a tool result carries as text.
type Result struct {
	Status    int    `json:"status"`
	ETag      string `json:"etag,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	// Body is the response body (a problem for an error); absent when there is none or it was cut.
	Body json.RawMessage `json:"body,omitempty"`
	// Truncated reports a body larger than the result cap; Bytes is its size.
	Truncated bool   `json:"truncated,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Note      string `json:"note,omitempty"`
}

// Execute runs a call. Bad arguments and API problems (4xx and 5xx) are tool errors (isError) the model can read; the
// returned error is only for a call that cannot be made at all (no principal or tool).
func (e *Executor) Execute(ctx context.Context, c Call) (*mcp.CallToolResult, error) {
	if c.Principal == nil || c.Tool == nil || e.Handler == nil {
		return nil, errors.New("spacemanmcp: call without principal, tool or handler")
	}
	req, err := e.request(ctx, c)
	if err != nil {
		return toolError(Result{Status: http.StatusBadRequest, Body: problemJSON(http.StatusBadRequest, "invalid", err.Error())}), nil
	}
	limit := e.MaxResultBytes
	if limit <= 0 {
		limit = DefaultMaxResultBytes
	}
	w := &capture{header: http.Header{}, limit: limit}
	e.Handler.ServeHTTP(w, req)
	if w.status == 0 {
		w.status = http.StatusOK
	}
	res := Result{Status: w.status, ETag: w.header.Get("ETag"), RequestID: w.header.Get(api.RequestIDHeader)}
	switch {
	case w.size > limit:
		res.Truncated, res.Bytes = true, w.size
		res.Note = fmt.Sprintf("the response was %d bytes, more than this server's %d-byte result limit: narrow the request (limit, cursor, filters)", w.size, limit)
	case w.size > 0 && json.Valid(w.body.Bytes()):
		res.Body = json.RawMessage(w.body.Bytes())
	case w.size > 0:
		res.Note = "the response was not JSON"
	}
	if w.status >= 400 {
		return toolError(res), nil
	}
	return toolResult(res, false), nil
}

func toolError(r Result) *mcp.CallToolResult { return toolResult(r, true) }

func toolResult(r Result, isError bool) *mcp.CallToolResult {
	b, err := json.Marshal(r)
	if err != nil {
		b = []byte(`{"status":500}`)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: isError}
}

func problemJSON(status int, code, detail string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"status": status, "code": code, "title": http.StatusText(status), "detail": detail})
	return b
}

// request builds the operation's HTTP request from the arguments.
func (e *Executor) request(ctx context.Context, c Call) (*http.Request, error) {
	t := c.Tool
	args := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(c.Args)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(c.Args))
		if err := dec.Decode(&args); err != nil {
			return nil, errors.New("arguments must be a JSON object")
		}
		if dec.More() {
			return nil, errors.New("arguments must be one JSON object")
		}
	}
	known := map[string]bool{}
	path := t.Path
	for _, name := range t.PathParams {
		known[name] = true
		v, ok, err := scalar(args, name)
		if err != nil {
			return nil, err
		}
		if !ok && name == "space_id" && c.DefaultSpaceID != "" {
			v, ok = c.DefaultSpaceID, true
		}
		if !ok || v == "" {
			return nil, fmt.Errorf("argument %q is required", name)
		}
		path = strings.ReplaceAll(path, "{"+name+"}", url.PathEscape(v))
	}
	q := url.Values{}
	for _, name := range t.QueryParams {
		known[name] = true
		v, ok, err := scalar(args, name)
		if err != nil {
			return nil, err
		}
		if ok {
			q.Set(name, v)
		}
	}
	var body []byte
	if t.Body {
		known[ArgBody] = true
		if raw, ok := args[ArgBody]; ok && !isNull(raw) {
			body = raw
		} else if t.BodyRequired {
			body = []byte("{}") // the operation's validation reports what is missing
		}
	}
	var ifMatch string
	if t.IfMatch {
		known[ArgIfMatch] = true
		v, ok, err := scalar(args, ArgIfMatch)
		if err != nil {
			return nil, err
		}
		if ok && v != "" {
			ifMatch = v
			if _, err := strconv.ParseInt(v, 10, 64); err == nil {
				ifMatch = `"` + v + `"`
			}
		}
	}
	var unknown []string
	for k := range args {
		if !known[k] {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown arguments: %s", strings.Join(unknown, ", "))
	}
	u := api.Prefix + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(api.WithInProcessPrincipal(ctx, c.Principal), t.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if t.Idempotent {
		key, err := IdempotencyKey(c.Principal, t, c.Args)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Idempotency-Key", key)
	}
	return req, nil
}

// IdempotencyKey derives a create's Idempotency-Key from the run, the token, the operation and the canonical
// arguments, so a retried call (the same arguments from the same run) returns the first call's result instead of
// creating a duplicate (ADR-0063, threat A8). Distinct arguments get distinct keys.
func IdempotencyKey(p *auth.Principal, t *Tool, args json.RawMessage) (string, error) {
	canon := []byte("null")
	if len(bytes.TrimSpace(args)) > 0 {
		var v any
		dec := json.NewDecoder(bytes.NewReader(args))
		dec.UseNumber()
		if err := dec.Decode(&v); err != nil {
			return "", errors.New("arguments must be a JSON object")
		}
		var err error
		if canon, err = json.Marshal(v); err != nil { // object keys sorted
			return "", err
		}
	}
	h := sha256.New()
	for _, part := range []string{"spaceman-mcp/v1", p.RunID, p.TokenID, t.OperationID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(canon)
	return "mcp-" + hex.EncodeToString(h.Sum(nil)), nil
}

// scalar returns an argument as the string a path, query or header value carries.
func scalar(args map[string]json.RawMessage, name string) (string, bool, error) {
	raw, ok := args[name]
	if !ok || isNull(raw) {
		return "", false, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "", false, fmt.Errorf("argument %q: %w", name, err)
	}
	switch x := v.(type) {
	case string:
		return x, true, nil
	case json.Number:
		return x.String(), true, nil
	case bool:
		return strconv.FormatBool(x), true, nil
	}
	return "", false, fmt.Errorf("argument %q must be a string, number or boolean", name)
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// capture is an in-memory ResponseWriter that keeps at most limit+1 body bytes and counts the rest.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
	size   int
	limit  int
}

func (w *capture) Header() http.Header { return w.header }

func (w *capture) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *capture) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if room := w.limit + 1 - w.body.Len(); room > 0 {
		w.body.Write(b[:min(len(b), room)])
	}
	w.size += len(b)
	return len(b), nil
}
