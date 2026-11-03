// Package client is a small typed client for the SpaceMan administrative API (/api/v1) used by the Terraform provider.
// It speaks the API's JSON directly: bearer authentication (API token or root token), an Idempotency-Key on every
// create, If-Match with the resource version on every update, and RFC 9457 problem documents turned into *APIError.
//
// It never logs request or response bodies: they can carry credentials (account secrets, MCP headers, Pulse creds,
// OIDC client secrets).
package client

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
	base      string // scheme://host[:port]/api/v1
	token     string
	http      *http.Client
	userAgent string
	// retryWait is the pause before the n-th retry (tests shorten it).
	retryWait func(attempt int) time.Duration
}

// Options configure a client.
type Options struct {
	// AllowInsecureHTTP permits plain http to a host that is not a loopback address. The bearer token would cross the
	// network in clear text, so this is off by default.
	AllowInsecureHTTP bool
	// Timeout bounds one HTTP request (default 60 s).
	Timeout time.Duration
	// HTTPClient replaces the default client (tests).
	HTTPClient *http.Client
	// UserAgent is sent as User-Agent.
	UserAgent string
}

// New returns a client for endpoint (scheme://host[:port], optionally ending in /api/v1) authenticating with token.
func New(endpoint, token string, o Options) (*Client, error) {
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("an API token is required")
	}
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("endpoint must be an absolute http(s) URL such as https://spaceman.example.com, got %q", endpoint)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("endpoint must not contain credentials, a query or a fragment")
	}
	if u.Scheme == "http" && !o.AllowInsecureHTTP && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("endpoint %s uses plain http to a non-loopback host; use https or set allow_insecure_http", u.Host)
	}
	p := strings.TrimSuffix(u.Path, "/")
	p = strings.TrimSuffix(p, APIPrefix)
	base := u.Scheme + "://" + u.Host + p + APIPrefix
	hc := o.HTTPClient
	if hc == nil {
		to := o.Timeout
		if to <= 0 {
			to = 60 * time.Second
		}
		hc = &http.Client{Timeout: to, CheckRedirect: func(*http.Request, []*http.Request) error {
			// Never forward the bearer token to wherever a redirect points.
			return http.ErrUseLastResponse
		}}
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "terraform-provider-spaceman"
	}
	return &Client{base: base, token: token, http: hc, userAgent: ua, retryWait: func(n int) time.Duration {
		return time.Duration(n*n) * 250 * time.Millisecond
	}}, nil
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

// FieldError is one invalid field of a problem.
type FieldError struct {
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// APIError is an RFC 9457 problem returned by SpaceMan, or a non-problem error status.
type APIError struct {
	Status int          `json:"status"`
	Code   string       `json:"code"`
	Title  string       `json:"title"`
	Detail string       `json:"detail"`
	Errors []FieldError `json:"errors"`
	// Operation is "METHOD /path" of the failed request (no query, no body).
	Operation string `json:"-"`
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d", e.Operation, e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	} else if e.Title != "" {
		b.WriteString(": " + e.Title)
	}
	for _, f := range e.Errors {
		if f.Field != "" {
			fmt.Fprintf(&b, "; %s: %s", f.Field, f.Message)
		} else {
			fmt.Fprintf(&b, "; %s", f.Message)
		}
	}
	return b.String()
}

// IsNotFound reports whether err is a 404 problem.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsConflict reports whether err is a version or uniqueness conflict (409, 412).
func IsConflict(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Status == http.StatusConflict || ae.Status == http.StatusPreconditionFailed)
}

// NewIdempotencyKey returns a random Idempotency-Key (32 hex characters).
func NewIdempotencyKey() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return "tf-" + hex.EncodeToString(b)
}

// request describes one call.
type request struct {
	method  string
	path    string // relative to the base, starting with /
	query   url.Values
	body    any
	ifMatch int64  // > 0: If-Match "<n>"
	idemKey string // non-empty: Idempotency-Key (and the request may be retried)
}

// do performs a request and decodes a JSON response into out (when non-nil). GETs and creates carrying an
// Idempotency-Key are retried on transport errors and 502/503/504: the key makes a replayed create return the
// original result instead of creating a second object.
func (c *Client) do(ctx context.Context, r request, out any) (http.Header, error) {
	var payload []byte
	if r.body != nil {
		var err error
		if payload, err = json.Marshal(r.body); err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
	}
	retryable := r.method == http.MethodGet || r.idemKey != ""
	attempts := 1
	if retryable {
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
		h, retry, err := c.once(ctx, r, payload, out)
		if err == nil {
			return h, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

func (c *Client) once(ctx context.Context, r request, payload []byte, out any) (http.Header, bool, error) {
	u := c.base + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json, application/problem+json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.ifMatch > 0 {
		req.Header.Set("If-Match", `"`+strconv.FormatInt(r.ifMatch, 10)+`"`)
	}
	if r.idemKey != "" {
		req.Header.Set("Idempotency-Key", r.idemKey)
	}
	op := r.method + " " + r.path
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, true, fmt.Errorf("%s: read response: %w", op, err)
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode, Operation: op}
		if json.Unmarshal(data, ae) != nil || ae.Status == 0 {
			ae.Status = resp.StatusCode
		}
		ae.Operation = op
		if ae.Title == "" {
			ae.Title = http.StatusText(resp.StatusCode)
		}
		retry := resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusGatewayTimeout
		return resp.Header, retry, ae
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, false, fmt.Errorf("%s: decode response: %w", op, err)
		}
	}
	return resp.Header, false, nil
}

func esc(s string) string { return url.PathEscape(s) }

func (c *Client) get(ctx context.Context, path string, out any) error {
	_, err := c.do(ctx, request{method: http.MethodGet, path: path}, out)
	return err
}

func (c *Client) create(ctx context.Context, path string, in, out any) error {
	_, err := c.do(ctx, request{method: http.MethodPost, path: path, body: in, idemKey: NewIdempotencyKey()}, out)
	return err
}

func (c *Client) post(ctx context.Context, path string, ifMatch int64, in, out any) error {
	_, err := c.do(ctx, request{method: http.MethodPost, path: path, body: in, ifMatch: ifMatch}, out)
	return err
}

func (c *Client) put(ctx context.Context, path string, ifMatch int64, in, out any) error {
	_, err := c.do(ctx, request{method: http.MethodPut, path: path, body: in, ifMatch: ifMatch}, out)
	return err
}

func (c *Client) patch(ctx context.Context, path string, ifMatch int64, in, out any) error {
	_, err := c.do(ctx, request{method: http.MethodPatch, path: path, body: in, ifMatch: ifMatch}, out)
	return err
}

// del deletes; a 404 counts as success (already gone).
func (c *Client) del(ctx context.Context, path string) error {
	_, err := c.do(ctx, request{method: http.MethodDelete, path: path}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
