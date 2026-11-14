package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// The Telegram channel (ADR-0083 §3.2–§3.4). A Telegram principal (auth.Principal.ViaTelegram) reaches the API two
// ways: the bot calls operations through InProcessHandler ("chat"), and the Mini App calls them over HTTP on its own
// host through TelegramHandler ("miniapp"). Every entry point has a TelegramClass: huma operations in
// TelegramPolicy.Ops, raw routes in RawRoute.Telegram, the change feed in TelegramPolicy.Watch. Anything without a
// class that admits the channel is refused, so a new operation or route is unreachable from Telegram until someone
// classifies it (and the classification test fails until then).

// TelegramClass is what an API entry point is for the Telegram channel. The zero value is unclassified: refused.
type TelegramClass string

// The classes.
const (
	// TelegramExcluded entry points are never served to Telegram.
	TelegramExcluded TelegramClass = "excluded"
	// TelegramChat entry points serve the bot only (in process).
	TelegramChat TelegramClass = "chat"
	// TelegramMiniApp entry points serve the Mini App only (HTTP on the Mini App host).
	TelegramMiniApp TelegramClass = "miniapp"
	// TelegramBoth entry points serve the bot and the Mini App.
	TelegramBoth TelegramClass = "both"
	// TelegramExchange marks the Mini App's session exchange: the only public operation served on the Mini App host,
	// and the only one an anonymous request reaches there. It is valid on public operations only.
	TelegramExchange TelegramClass = "exchange"
)

// TelegramClasses lists every valid class.
var TelegramClasses = []TelegramClass{TelegramExcluded, TelegramChat, TelegramMiniApp, TelegramBoth, TelegramExchange}

// TelegramScheme is the Authorization scheme of a Mini App session ("Authorization: Telegram <token>"). Only the Mini
// App host accepts it; the main host answers it with 401 before looking at the cookie.
const TelegramScheme = "Telegram"

// TelegramFeatureTerminalWatch is the per-space setting a Telegram principal needs for the live terminal stream.
const TelegramFeatureTerminalWatch = "terminal_watch"

// TelegramSessions resolves Mini App session tokens to Telegram principals (TG-9a supplies it). Authenticate marks
// the session active; Validate (stream re-checks) does not.
type TelegramSessions interface {
	Authenticate(ctx context.Context, token string) (*auth.Principal, error)
	Validate(ctx context.Context, token string) (*auth.Principal, error)
}

// TelegramPolicy is the Telegram channel's configuration. A nil policy, like any nil field, refuses: no operation is
// classified, no session authenticates, no space feature is on and no Mini App mutation passes the origin check.
type TelegramPolicy struct {
	// Ops classifies huma operations by operation ID (telegram.Ops).
	Ops map[string]TelegramClass
	// Watch classifies the change feed, GET /api/v1/watch.
	Watch TelegramClass
	// Sessions resolves the Mini App's session header.
	Sessions TelegramSessions
	// SpaceFeature reports whether a space has a Telegram feature on (TelegramFeatureTerminalWatch).
	SpaceFeature func(ctx context.Context, spaceID, feature string) bool
	// MiniAppOrigin is the Mini App's origin, "https://<miniapp_host>": Mini App mutations must come from it.
	MiniAppOrigin func() string
}

// telegramChannel is how the request reached the API: not through a Telegram entry point, from the bot in process,
// or from the Mini App over HTTP.
type telegramChannel int

const (
	channelNone telegramChannel = iota
	channelChat
	channelMiniApp
)

type (
	telegramChannelKey struct{}
	telegramTokenKey   struct{}
)

// telegramChannelFrom returns the request's channel. Only InProcessHandler (chat) and TelegramHandler (miniapp) set
// it; nothing a client sends can.
func telegramChannelFrom(ctx context.Context) telegramChannel {
	ch, _ := ctx.Value(telegramChannelKey{}).(telegramChannel)
	return ch
}

func telegramTokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(telegramTokenKey{}).(string)
	return t
}

// allows reports whether class c admits channel ch.
func (c TelegramClass) allows(ch telegramChannel) bool {
	switch ch {
	case channelChat:
		return c == TelegramChat || c == TelegramBoth
	case channelMiniApp:
		return c == TelegramMiniApp || c == TelegramBoth
	}
	return false
}

func errTelegramRefused() *Problem {
	return newProblem(http.StatusForbidden, apperr.CodeForbidden, "this operation is not available from Telegram", nil)
}

// telegramGate applies an entry point's class to a request: nil when the request is not a Telegram one at all (no
// Telegram channel and no Telegram principal) or when the class admits its channel. On a Telegram channel only a
// Telegram principal counts as authenticated, and a Telegram principal off a Telegram channel admits nothing.
func telegramGate(ch telegramChannel, p *auth.Principal, class TelegramClass) *Problem {
	if ch == channelNone && !p.ViaTelegram() {
		return nil
	}
	if !p.ViaTelegram() {
		return errUnauthenticated()
	}
	if !class.allows(ch) {
		return errTelegramRefused()
	}
	return nil
}

// telegramOpRefusal applies the Telegram classification to a huma operation (authorizeOps). Public operations are
// served on a Telegram channel only when they are the Mini App's session exchange: anything else public answers 401 on
// the Mini App host (anonymous requests reach only the exchange) and 403 to the bot.
func (d *Deps) telegramOpRefusal(opID string, rl Rule, p *auth.Principal, ch telegramChannel) *Problem {
	var class TelegramClass
	if d.Telegram != nil {
		class = d.Telegram.Ops[opID]
	}
	if rl.Public {
		switch {
		case ch == channelMiniApp && class == TelegramExchange:
			return nil
		case ch == channelMiniApp:
			return errUnauthenticated()
		}
		return errTelegramRefused()
	}
	if p == nil {
		return errUnauthenticated()
	}
	return telegramGate(ch, p, class)
}

// telegramFeature reports whether a space has a Telegram feature on (nil policy or callback: off).
func (d *Deps) telegramFeature(ctx context.Context, spaceID, feature string) bool {
	return d.Telegram != nil && d.Telegram.SpaceFeature != nil && d.Telegram.SpaceFeature(ctx, spaceID, feature)
}

// TelegramHandler returns the API handler in Telegram mode, for the Mini App host only (ADR-0083 §3.4.1): the same
// operations, raw routes and change feed as Handler, with these differences.
//   - The Cookie request header is deleted before authentication, and no response carries Set-Cookie.
//   - Only "Authorization: Telegram <token>" authenticates; any other Authorization (Bearer API or root tokens
//     included), several Authorization headers, or a session that doesn't resolve is 401. No header is anonymous,
//     which reaches only the session exchange (the OpenAPI documents and JSON schemas are 404 there).
//   - Mutations must come from the Mini App origin (TelegramPolicy.MiniAppOrigin), not the public URL.
//   - The external MCP endpoint is not served.
func (d *Deps) TelegramHandler() http.Handler {
	return d.handler(true)
}

// telegramMode marks a request as the Mini App's, drops its cookies and keeps Set-Cookie off its response.
func (d *Deps) telegramMode(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSpecPath(r.URL.Path) {
			// huma serves its OpenAPI documents and JSON schemas without authentication; the Mini App host serves no
			// anonymous request but the session exchange, so they are not found there (the site publishes the spec).
			writeProblem(w, newProblem(http.StatusNotFound, "not_found", "not found", nil))
			return
		}
		r.Header.Del("Cookie")
		ctx := context.WithValue(r.Context(), telegramChannelKey{}, channelMiniApp)
		next.ServeHTTP(&noCookieWriter{ResponseWriter: w, d: d}, r.WithContext(ctx))
	})
}

// isSpecPath reports whether a path is one of huma's anonymous documentation routes: the OpenAPI documents
// ("/openapi.json", ".yaml", "-3.0.json", "-3.0.yaml", see OpenAPIPath) and the JSON schemas.
func isSpecPath(path string) bool {
	return strings.HasPrefix(path, Prefix+"/openapi") || strings.HasPrefix(path, Prefix+"/schemas/")
}

// noCookieWriter drops Set-Cookie from a Mini App response. Nothing on that host sets a cookie; a drop is a bug and
// is logged.
type noCookieWriter struct {
	http.ResponseWriter
	d       *Deps
	written bool
}

func (w *noCookieWriter) strip() {
	if w.written {
		return
	}
	w.written = true
	if _, ok := w.Header()["Set-Cookie"]; ok {
		w.Header().Del("Set-Cookie")
		w.d.log().Warn("dropped Set-Cookie from a Mini App response")
	}
}

func (w *noCookieWriter) WriteHeader(code int) {
	w.strip()
	w.ResponseWriter.WriteHeader(code)
}

func (w *noCookieWriter) Write(b []byte) (int, error) {
	w.strip()
	return w.ResponseWriter.Write(b)
}

// Flush keeps streaming responses (the change feed, the terminal) working through the wrapper.
func (w *noCookieWriter) Flush() {
	w.strip()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *noCookieWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// authorizationScheme returns the scheme of an Authorization header value and the rest.
func authorizationScheme(v string) (scheme, rest string) {
	scheme, rest, _ = strings.Cut(strings.TrimSpace(v), " ")
	return scheme, strings.TrimSpace(rest)
}

// hasTelegramScheme reports whether any Authorization header uses the Telegram scheme (case-insensitive, as HTTP
// schemes are).
func hasTelegramScheme(r *http.Request) bool {
	for _, v := range r.Header.Values("Authorization") {
		if s, _ := authorizationScheme(v); strings.EqualFold(s, TelegramScheme) {
			return true
		}
	}
	return false
}

// authenticateTelegram is authenticate on the Mini App host. It returns the principal (nil: anonymous) with its session
// token, and false when it already wrote the refusal.
func (d *Deps) authenticateTelegram(w http.ResponseWriter, r *http.Request, limit *ipLimiter) (*auth.Principal, string, bool) {
	ctx := r.Context()
	vals := r.Header.Values("Authorization")
	if len(vals) == 0 {
		return nil, "", true
	}
	fail := func(err error) (*auth.Principal, string, bool) {
		if !limit.allow(ClientIP(ctx)) {
			writeProblem(w, problemFor(errRateLimited))
			return nil, "", false
		}
		if err == nil || errors.Is(err, auth.ErrUnauthorized) {
			writeProblem(w, errUnauthenticated())
			return nil, "", false
		}
		var pr *Problem
		if !errors.As(d.fail(ctx, "authenticate_telegram", err), &pr) {
			pr = newProblem(http.StatusInternalServerError, "internal_error", "internal error", nil)
		}
		writeProblem(w, pr)
		return nil, "", false
	}
	scheme, tok := authorizationScheme(vals[0])
	if len(vals) != 1 || !strings.EqualFold(scheme, TelegramScheme) || tok == "" || len(tok) > 256 || d.Telegram == nil || d.Telegram.Sessions == nil {
		return fail(nil)
	}
	p, err := d.Telegram.Sessions.Authenticate(ctx, tok)
	if err != nil {
		return fail(err)
	}
	if !p.ViaTelegram() || p.TokenID != "" || p.RootTokenID != "" || p.User == nil {
		// A resolver that returns anything but a Telegram principal is refused: the Mini App host serves nothing else.
		return fail(nil)
	}
	return p, tok, true
}

// miniAppSameOrigin is sameOrigin on the Mini App host: Origin must be the Mini App's origin (or, without Origin,
// Sec-Fetch-Site same-origin). No configured origin refuses every mutation.
func (d *Deps) miniAppSameOrigin(r *http.Request) bool {
	want := ""
	if d.Telegram != nil && d.Telegram.MiniAppOrigin != nil {
		if u, err := url.Parse(d.Telegram.MiniAppOrigin()); err == nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http") {
			want = u.Scheme + "://" + u.Host
		}
	}
	if want == "" {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		return strings.EqualFold(o, want)
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// revalidateTelegram re-checks a Mini App stream's session without marking it active: the session still resolves, to
// the same link, and isn't Limited.
func (d *Deps) revalidateTelegram(ctx context.Context, p *auth.Principal) (*auth.Principal, bool) {
	if d.Telegram == nil || d.Telegram.Sessions == nil {
		return nil, false
	}
	np, err := d.Telegram.Sessions.Validate(ctx, telegramTokenFrom(ctx))
	if err != nil || !np.ViaTelegram() || np.TelegramLinkID != p.TelegramLinkID || np.TokenID != "" || np.RootTokenID != "" || np.Limited {
		return nil, false
	}
	return np, true
}
