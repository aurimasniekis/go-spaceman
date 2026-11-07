// Package harborpilot integrates the Harbor Pilot LLM gateway (ADR-0060, SM-GW-*; contract
// docs/contracts/harbor-pilot.md, v0.1.3). Central holds one connection (a base URL, a parent token with tokens:mint and
// an optional read-only service-principal token, all sealed and write-only), spaces opt in per harness, and each run
// gets its own child token.
//
// This file is central's HTTP client. It never follows redirects (a redirect would forward a token elsewhere), needs
// https unless the host is loopback or insecure HTTP is explicitly allowed, keeps only Harbor Pilot's status, error type
// and a bounded, sanitised error message (never a body), and never logs or returns a token in an error.
package harborpilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// MinVersion is the oldest Harbor Pilot SpaceMan supports (ADR-0060 decision 8).
const MinVersion = "0.1.3"

// Scopes.
const (
	ScopeInvoke = "gateway:invoke"
	ScopeModels = "gateway:models"
	ScopeMint   = "tokens:mint"
)

// Class is the kind of a Harbor Pilot failure.
type Class string

// Failure classes. Only Transport, Upstream and RateLimited are worth retrying.
const (
	ClassUnauthorized    Class = "unauthorized"      // 401: the token is invalid, expired or revoked
	ClassLacksMint       Class = "lacks_mint"        // 403: the parent token has no tokens:mint
	ClassChildCannotMint Class = "child_cannot_mint" // 403: a child token was given as the parent
	ClassForbidden       Class = "forbidden"         // 403: any other permission refusal
	ClassInvalid         Class = "invalid"           // 400/422: Harbor Pilot refused the request's content
	ClassTooOld          Class = "too_old"           // 400: the gateway is older than MinVersion
	ClassNotFound        Class = "not_found"         // 404
	ClassBudgetExceeded  Class = "budget_exceeded"   // 429 budget_exceeded (mint with check_budget)
	ClassRateLimited     Class = "rate_limited"      // 429 otherwise
	ClassUpstream        Class = "upstream"          // 5xx
	ClassTransport       Class = "transport"         // the request did not complete (network, timeout)
	ClassProtocol        Class = "protocol"          // an unexpected status, a redirect or an unreadable answer
)

// Error is a Harbor Pilot failure. Message is Harbor Pilot's own error message, cut to a fixed bound and sanitised: it
// is untrusted text (T-03), shown only as part of a reason.
type Error struct {
	Op       string // mint, revoke, budget_status, get_project
	Class    Class
	Status   int    // HTTP status; 0 for a transport failure
	Type     string // Harbor Pilot's error type (gateway routes), e.g. permission_error
	Code     string // Harbor Pilot's error code when it sends one, e.g. budget_exceeded
	Message  string
	BudgetID string // the refusing budget, for ClassBudgetExceeded
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString("harbor pilot ")
	b.WriteString(e.Op)
	b.WriteString(": ")
	b.WriteString(e.Reason())
	return b.String()
}

// Reason is a readable explanation for a person (a refusal, a run's end reason, a validation error).
func (e *Error) Reason() string {
	suffix := ""
	if e.Message != "" {
		suffix = " (Harbor Pilot: " + e.Message + ")"
	}
	switch e.Class {
	case ClassUnauthorized:
		return "the token is invalid, expired or revoked" + suffix
	case ClassLacksMint:
		return "parent token lacks " + ScopeMint
	case ClassChildCannotMint:
		return "the parent token is a child credential, which cannot mint; issue a parent credential with " + ScopeMint + " on the client binding"
	case ClassForbidden:
		return "permission denied" + suffix
	case ClassTooOld:
		return "the gateway is older than Harbor Pilot " + MinVersion + ", which SpaceMan needs" + suffix
	case ClassNotFound:
		return "not found" + suffix
	case ClassBudgetExceeded:
		return "a blocking Harbor Pilot budget is used up" + suffix
	case ClassRateLimited:
		return "rate limited" + suffix
	case ClassUpstream:
		return fmt.Sprintf("Harbor Pilot failed with status %d", e.Status) + suffix
	case ClassTransport:
		return "Harbor Pilot is unreachable" + suffix
	case ClassInvalid:
		return "Harbor Pilot refused the request" + suffix
	}
	if e.Status != 0 {
		return fmt.Sprintf("unexpected answer (status %d)", e.Status) + suffix
	}
	return "unexpected answer" + suffix
}

// Retryable reports whether the same request may succeed later.
func (e *Error) Retryable() bool {
	return e.Class == ClassTransport || e.Class == ClassUpstream || e.Class == ClassRateLimited
}

// IsClass reports whether err is a Harbor Pilot error of class c.
func IsClass(err error, c Class) bool {
	var he *Error
	return errors.As(err, &he) && he.Class == c
}

// ParseBaseURL checks a gateway origin: an absolute http(s) URL without credentials, query or fragment. Plain http is
// accepted only when allowInsecure is set, or when loopbackExempt is set and the host is a loopback address. The
// returned URL has no trailing slash.
func ParseBaseURL(raw string, allowInsecure, loopbackExempt bool) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return nil, errors.New("must be an absolute http(s) URL such as https://harbor-pilot.example.com")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, errors.New("must not contain credentials, a query or a fragment")
	}
	if u.Scheme == "http" && !allowInsecure && (!loopbackExempt || !isLoopback(u.Hostname())) {
		if loopbackExempt {
			return nil, errors.New("uses plain http to a host that is not a loopback address: use https, or allow insecure HTTP (tokens then cross the network in clear text)")
		}
		return nil, errors.New("uses plain http: runs send their tokens to this URL, so it needs https unless insecure HTTP is allowed")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

func isLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Client calls one Harbor Pilot gateway. Tokens are passed per call; the client keeps none.
type Client struct {
	base string
	hc   *http.Client
}

// ClientOptions tune a client.
type ClientOptions struct {
	AllowInsecureHTTP bool
	// Transport replaces the HTTP transport (tests). nil: a clone of http.DefaultTransport.
	Transport http.RoundTripper
	// Timeout bounds every request (default 15 s).
	Timeout time.Duration
}

// NewClient returns a client for the gateway at baseURL (checked with ParseBaseURL, loopback exempt).
func NewClient(baseURL string, o ClientOptions) (*Client, error) {
	u, err := ParseBaseURL(baseURL, o.AllowInsecureHTTP, true)
	if err != nil {
		return nil, err
	}
	to := o.Timeout
	if to <= 0 {
		to = 15 * time.Second
	}
	tr := o.Transport
	if tr == nil {
		tr = newTransport()
	}
	return &Client{base: u.String(), hc: &http.Client{Timeout: to, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// newTransport is the default transport: no proxy (a proxy from the environment would see every token, in clear text
// with insecure HTTP), and a dial check that refuses addresses no gateway has: link-local (cloud metadata services),
// unspecified and multicast. It is checked on the resolved address, so a name that resolves there is refused too.
// Private and loopback addresses stay allowed, since a Harbor Pilot gateway usually runs on an internal network.
func newTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: dialControl}
	tr.DialContext = dialer.DialContext
	return tr
}

func dialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if deniedAddr(ap.Addr()) {
		return errDeniedAddr
	}
	return nil
}

var errDeniedAddr = errors.New("harbor pilot: the gateway address is link-local, unspecified, multicast or broadcast")

// deniedPrefixes are the ranges central never calls a gateway on, matching the MCP gateway's always-denied list
// (internal/gateway/egress.go): "this network", link-local (cloud metadata at 169.254.169.254), multicast, broadcast,
// IPv6 link-local, multicast and unspecified, plus AWS's IPv6 metadata address.
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("ff00::/8"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("fd00:ec2::254/128"),
}

// IPv6 prefixes that carry an IPv4 address: NAT64 (RFC 6052, its local-use RFC 8215 prefix) in the last 32 bits, and
// 6to4 (RFC 3056) in bits 16-47. A denied IPv4 address is denied through them too.
var (
	nat64Prefixes = []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48")}
	sixToFour     = netip.MustParsePrefix("2002::/16")
)

// deniedAddr reports the addresses central never calls a gateway on.
func deniedAddr(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range deniedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	if a.Is6() {
		b := a.As16()
		for _, p := range nat64Prefixes {
			if p.Contains(a) {
				return deniedAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
			}
		}
		if sixToFour.Contains(a) {
			return deniedAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified()
}

// BaseURL is the gateway origin the client calls.
func (c *Client) BaseURL() string { return c.base }

// MintRequest is the body of POST /v1/harbor/credentials (Harbor Pilot refuses unknown fields). CheckBudget is always
// sent, also when false: a gateway older than 0.1.3 does not know the field and refuses it, which is how an older
// gateway is detected (ADR-0060 decision 8).
type MintRequest struct {
	Name        string            `json:"name"`
	Scopes      []string          `json:"scopes"`
	TTLSeconds  int64             `json:"ttl_seconds,omitempty"`
	BoundRefs   map[string]string `json:"bound_refs,omitempty"`
	CheckBudget bool              `json:"check_budget"`
}

// Credential is a minted child credential (never its token).
type Credential struct {
	ID        string            `json:"id"`
	BindingID string            `json:"binding_id"`
	ParentID  string            `json:"parent_id"`
	Scopes    []string          `json:"scopes"`
	BoundRefs map[string]string `json:"bound_refs"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// Mint mints a child credential with the parent token and returns it with the child's token.
func (c *Client) Mint(ctx context.Context, parentToken string, in MintRequest) (*Credential, string, error) {
	var out struct {
		Credential Credential `json:"credential"`
		Token      string     `json:"token"`
	}
	if err := c.do(ctx, "mint", http.MethodPost, "/v1/harbor/credentials", parentToken, in, http.StatusCreated, &out); err != nil {
		return nil, "", err
	}
	if out.Credential.ID == "" || !strings.HasPrefix(out.Token, "hpk_") {
		return nil, "", &Error{Op: "mint", Class: ClassProtocol, Status: http.StatusCreated, Message: "the answer has no credential id or token"}
	}
	return &out.Credential, out.Token, nil
}

// Revoke revokes a child credential with its parent's token. Revoking an already revoked credential succeeds (204);
// an unknown one is ClassNotFound.
func (c *Client) Revoke(ctx context.Context, parentToken, credentialID string) error {
	return c.do(ctx, "revoke", http.MethodDelete, "/v1/harbor/credentials/"+url.PathEscape(credentialID), parentToken, nil, http.StatusNoContent, nil)
}

// Budget is one item of GET /api/v1/budgets/status. Amounts are in 1e-10 of the currency.
type Budget struct {
	BudgetID    string    `json:"budget_id"`
	Name        string    `json:"name"`
	Scope       string    `json:"scope"` // global, workspace, project, binding or account
	ScopeID     string    `json:"scope_id"`
	Period      string    `json:"period"`
	PeriodStart time.Time `json:"period_start"`
	PeriodEnd   time.Time `json:"period_end"`
	Currency    string    `json:"currency"`
	Enforcement string    `json:"enforcement"` // block or warn
	Enabled     bool      `json:"enabled"`
	LimitE10    int64     `json:"limit_e10"`
	SpentE10    int64     `json:"spent_e10"`
	ReservedE10 int64     `json:"reserved_e10"`
	Fraction    float64   `json:"fraction"`
	State       string    `json:"state"` // ok, warning, exceeded or disabled
}

// BudgetStatus reads every budget's current period (service-principal token with usage.read).
func (c *Client) BudgetStatus(ctx context.Context, serviceToken string) ([]Budget, error) {
	var out struct {
		Items []Budget `json:"items"`
	}
	if err := c.do(ctx, "budget_status", http.MethodGet, "/api/v1/budgets/status", serviceToken, nil, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// Project is a Harbor Pilot project.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Spec struct {
		WorkspaceID string `json:"workspace_id"`
		DisplayName string `json:"display_name"`
	} `json:"spec"`
}

// GetProject reads a project by id (Harbor Pilot also accepts a name here; callers compare the returned id) with a
// service-principal token holding config.read.
func (c *Client) GetProject(ctx context.Context, serviceToken, id string) (*Project, error) {
	var p Project
	if err := c.do(ctx, "get_project", http.MethodGet, "/api/v1/projects/"+url.PathEscape(id), serviceToken, nil, http.StatusOK, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

const maxBody = 1 << 20

func (c *Client) do(ctx context.Context, op, method, path, token string, body any, want int, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return &Error{Op: op, Class: ClassProtocol, Message: "could not build the request"}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spaceman-central")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		msg := "the request failed"
		var ne net.Error
		switch {
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
			msg = "the request timed out"
		case errors.Is(err, context.Canceled):
			msg = "the request was cancelled"
		case errors.Is(err, errDeniedAddr):
			msg = "the gateway address is link-local, unspecified, multicast or broadcast (cloud metadata included), which central never calls"
		}
		// The underlying error can name the URL; never the token. Keep only a fixed text.
		return &Error{Op: op, Class: ClassTransport, Message: msg}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return &Error{Op: op, Class: ClassTransport, Status: resp.StatusCode, Message: "the answer could not be read"}
	}
	if resp.StatusCode == want {
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{Op: op, Class: ClassProtocol, Status: resp.StatusCode, Message: "the answer is not the expected JSON"}
		}
		return nil
	}
	return classify(op, resp.StatusCode, data)
}

// classify turns a non-success answer into an Error. Harbor Pilot's gateway routes answer in the OpenAI shape
// {"error":{message,type,code}}; its control-plane API answers problem+json {title,detail}.
func classify(op string, status int, body []byte) *Error {
	e := &Error{Op: op, Status: status}
	var env struct {
		Error *struct {
			Message  string `json:"message"`
			Type     string `json:"type"`
			Code     any    `json:"code"`
			BudgetID string `json:"budget_id"`
		} `json:"error"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &env) == nil {
		switch {
		case env.Error != nil:
			e.Type = bounded(Sanitize(env.Error.Type), 64)
			if s, ok := env.Error.Code.(string); ok {
				e.Code = bounded(Sanitize(s), 64)
			}
			e.Message = Sanitize(env.Error.Message)
			e.BudgetID = bounded(Sanitize(env.Error.BudgetID), 64)
		case env.Detail != "":
			e.Message = Sanitize(env.Detail)
		case env.Title != "":
			e.Message = Sanitize(env.Title)
		}
	}
	switch {
	case status >= 300 && status < 400:
		e.Class, e.Message = ClassProtocol, "redirects are not followed"
	case status == http.StatusUnauthorized:
		e.Class = ClassUnauthorized
	case status == http.StatusForbidden:
		switch {
		case strings.Contains(e.Message, "lacks "+ScopeMint):
			e.Class = ClassLacksMint
		case strings.Contains(e.Message, "cannot mint"):
			e.Class = ClassChildCannotMint
		default:
			e.Class = ClassForbidden
		}
	case status == http.StatusNotFound:
		e.Class = ClassNotFound
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		e.Class = ClassInvalid
		if strings.Contains(e.Message, `unknown field "check_budget"`) {
			e.Class = ClassTooOld
		}
	case status == http.StatusTooManyRequests:
		e.Class = ClassRateLimited
		if e.Code == "budget_exceeded" {
			e.Class = ClassBudgetExceeded
		}
	case status >= 500:
		e.Class = ClassUpstream
	default:
		e.Class = ClassProtocol
	}
	return e
}

// Token-shaped strings: Harbor Pilot's gateway (hpk_) and API (hpa_) tokens.
var reToken = regexp.MustCompile(`hp[a-z]_[A-Za-z0-9_\-]+`)

const maxMessage = 300

// Sanitize bounds untrusted text from Harbor Pilot: control characters become spaces, anything token-shaped is
// replaced, and the result is cut to a fixed length.
func Sanitize(s string) string {
	s = reToken.ReplaceAllStringFunc(s, func(t string) string { return t[:4] + "…" })
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return bounded(strings.Join(strings.Fields(s), " "), maxMessage)
}

func bounded(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
