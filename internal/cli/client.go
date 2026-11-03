// Package cli is SpaceMan's remote command-line client (SM-API-003, ADR-0040): commands that call a running central's
// administrative API (/api/v1) with an API token or the root token. Every command is bound to an operation of the
// OpenAPI document of its own build by operation ID; nothing here bypasses the API.
//
// Secrets (tokens, account secrets, MCP headers, Pulse credentials, passwords) are read from files, standard input or
// the environment, never from flag values, and the client never logs request or response bodies.
package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// APIPrefix is the versioned base path of the administrative API.
const APIPrefix = "/api/v1"

// Client calls one SpaceMan central.
type Client struct {
	base      string // scheme://host[:port][/prefix]/api/v1
	token     string
	http      *http.Client
	userAgent string
	retryWait func(attempt int) time.Duration
}

// ClientOptions configure a client.
type ClientOptions struct {
	// AllowInsecureHTTP permits plain http to a host that is not a loopback address (the token would cross the network
	// in clear text). Off by default, as in the Terraform provider.
	AllowInsecureHTTP bool
	// Timeout bounds one HTTP request (default 60 s).
	Timeout time.Duration
	// Transport replaces the default transport (tests).
	Transport http.RoundTripper
	// UserAgent is sent as User-Agent.
	UserAgent string
	// Anonymous allows a client without a token (public operations such as system info).
	Anonymous bool
}

// NewClient returns a client for server (scheme://host[:port], optionally ending in /api/v1) using token.
func NewClient(server, token string, o ClientOptions) (*Client, error) {
	if strings.TrimSpace(token) == "" && !o.Anonymous {
		return nil, &AuthError{Msg: "no API token: set --token-file, SPACEMAN_TOKEN_FILE or SPACEMAN_TOKEN"}
	}
	if strings.ContainsAny(token, "\r\n") {
		return nil, &AuthError{Msg: "the API token contains a line break; check the token file"}
	}
	server = strings.TrimSpace(server)
	if server == "" {
		return nil, usageErrorf("no server: set --server or SPACEMAN_SERVER (for example https://spaceman.example.com)")
	}
	u, err := url.Parse(server)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, usageErrorf("--server must be an absolute http(s) URL such as https://spaceman.example.com, got %q", server)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, usageErrorf("--server must not contain credentials, a query or a fragment")
	}
	if u.Scheme == "http" && !o.AllowInsecureHTTP && !isLoopback(u.Hostname()) {
		return nil, usageErrorf("--server %s uses plain http to a non-loopback host; use https (or --allow-insecure-http, which sends the token in clear text)", u.Host)
	}
	p := strings.TrimSuffix(u.Path, "/")
	p = strings.TrimSuffix(p, APIPrefix)
	to := o.Timeout
	if to <= 0 {
		to = 60 * time.Second
	}
	hc := &http.Client{Timeout: to, Transport: o.Transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		// Never forward the bearer token to wherever a redirect points.
		return http.ErrUseLastResponse
	}}
	ua := o.UserAgent
	if ua == "" {
		ua = "spaceman-cli"
	}
	return &Client{base: u.Scheme + "://" + u.Host + p + APIPrefix, token: token, http: hc, userAgent: ua,
		retryWait: func(n int) time.Duration { return time.Duration(n*n) * 250 * time.Millisecond }}, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// BaseURL returns the API base URL (…/api/v1).
func (c *Client) BaseURL() string { return c.base }

// NewIdempotencyKey returns a random Idempotency-Key.
func NewIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "cli-" + hex.EncodeToString(b)
}

// Request is one API call. Path is relative to /api/v1 and already has its parameters substituted.
type Request struct {
	Method  string
	Path    string
	Query   url.Values
	Body    any
	IfMatch string // a version; sent as If-Match: "<version>"
	IdemKey string // sent as Idempotency-Key; the request may then be retried
	// Raw replaces the JSON body (a multipart upload) and is sent with ContentType.
	Raw         []byte
	ContentType string
	// Accept replaces the default Accept header (event streams, downloads).
	Accept string
	// Op names the operation in errors.
	Op string
}

// Response is a successful API response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Object decodes a JSON object body (nil for an empty body).
func (r *Response) Object() (map[string]any, error) {
	if r == nil || len(bytes.TrimSpace(r.Body)) == 0 {
		return nil, nil
	}
	var m map[string]any
	d := json.NewDecoder(bytes.NewReader(r.Body))
	d.UseNumber()
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return m, nil
}

// Do performs a request. GETs and creates carrying an Idempotency-Key are retried on transport errors and 502/503/504:
// the key makes a replayed create return the original result instead of creating a second object.
func (c *Client) Do(ctx context.Context, r Request) (*Response, error) {
	var payload []byte
	if r.Raw != nil {
		payload = r.Raw
	} else if r.Body != nil {
		var err error
		if payload, err = json.Marshal(r.Body); err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	attempts := 1
	if r.Method == http.MethodGet || r.IdemKey != "" {
		attempts = 4
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(c.retryWait(i)):
			}
		}
		res, retry, err := c.once(ctx, r, payload)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) newRequest(ctx context.Context, r Request, payload []byte) (*http.Request, error) {
	u := c.base + r.Path
	if len(r.Query) > 0 {
		u += "?" + r.Query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, u, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	accept := r.Accept
	if accept == "" {
		accept = "application/json, application/problem+json"
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", c.userAgent)
	switch {
	case r.ContentType != "" && payload != nil:
		req.Header.Set("Content-Type", r.ContentType)
	case payload != nil:
		req.Header.Set("Content-Type", "application/json")
	}
	if r.IfMatch != "" {
		req.Header.Set("If-Match", `"`+r.IfMatch+`"`)
	}
	if r.IdemKey != "" {
		req.Header.Set("Idempotency-Key", r.IdemKey)
	}
	return req, nil
}

func (c *Client) once(ctx context.Context, r Request, payload []byte) (*Response, bool, error) {
	req, err := c.newRequest(ctx, r, payload)
	if err != nil {
		return nil, false, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, &NetError{Op: r.Method + " " + r.Path, Err: stripURL(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, true, &NetError{Op: r.Method + " " + r.Path, Err: err}
	}
	if resp.StatusCode >= 300 {
		ae, retry := problemOf(resp, data, r)
		return nil, retry, ae
	}
	return &Response{Status: resp.StatusCode, Header: resp.Header, Body: data}, false, nil
}

// Open performs a request whose response body the caller streams (a download or an event stream): it is never retried
// and has no overall timeout (cancel the context to stop it). The caller closes the body.
func (c *Client) Open(ctx context.Context, r Request) (*http.Response, error) {
	req, err := c.newRequest(ctx, r, nil)
	if err != nil {
		return nil, err
	}
	hc := *c.http
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &NetError{Op: r.Method + " " + r.Path, Err: stripURL(err)}
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		ae, _ := problemOf(resp, data, r)
		return nil, ae
	}
	return resp, nil
}

// problemOf turns an error response into an APIError and reports whether the status is worth a retry.
func problemOf(resp *http.Response, data []byte, r Request) (*APIError, bool) {
	ae := &APIError{Status: resp.StatusCode}
	if json.Unmarshal(data, ae) != nil || ae.Status == 0 {
		ae.Status = resp.StatusCode
	}
	ae.Method, ae.Path, ae.Op, ae.RequestID = r.Method, r.Path, r.Op, resp.Header.Get("X-Request-Id")
	if ae.Title == "" {
		ae.Title = http.StatusText(resp.StatusCode)
	}
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		ae.Detail = "the server answered with a redirect, which is never followed (the token stays with --server); check the URL"
		if loc := resp.Header.Get("Location"); loc != "" {
			ae.Detail += " (Location: " + loc + ")"
		}
	}
	retry := resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
	return ae, retry
}

// stripURL removes the request URL from a transport error (it never carries the token, but keeps messages short).
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// versionOf returns the resource version of a response: its ETag, else the body's "version" field.
func versionOf(r *Response, obj map[string]any) string {
	if r != nil {
		if et := normalizeVersion(r.Header.Get("ETag")); et != "" {
			return et
		}
	}
	return numberString(obj["version"])
}

// normalizeVersion accepts 3, "3" and W/"3".
func normalizeVersion(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "W/"))
	return strings.Trim(v, `"`)
}

func numberString(v any) string {
	switch n := v.(type) {
	case json.Number:
		return n.String()
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case int:
		return strconv.Itoa(n)
	case string:
		return n
	}
	return ""
}
