// Package api is SpaceMan's administrative HTTP API (SM-API-001/002): huma v2 on net/http, OpenAPI 3.1 generated from
// the Go types (ADR-0003), RFC 9457 problems, cookie sessions with CSRF for the web UI, bearer API and root tokens for
// automation, Casbin authorization per operation (ADR-0004), optimistic concurrency, idempotency keys, cursor pagination
// and a durable change-feed watch.
//
// Every operation has exactly one authorization rule (rules.go); an operation without one is unreachable, and
// TestEveryOperationHasARule enforces the table.
package api

import (
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/authz"
	"github.com/aurimasniekis/go-spaceman/internal/buildinfo"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
)

func init() {
	// Handlers return non-nil slices, so the spec (and the generated TypeScript types) can say arrays are never null.
	huma.DefaultArrayNullable = false
}

// Prefix is the API base path. The v1 API evolves additively; breaking changes need /api/v2.
const Prefix = "/api/v1"

// Deps are the services the API uses. Spec generation passes a zero Deps: handlers must not run then.
type Deps struct {
	Log     *slog.Logger
	Store   *store.Store
	Auth    *auth.Service
	Authz   *authz.Engine
	Audit   *audit.Writer
	Bus     *eventbus.Bus
	Metrics *metrics.Metrics
	Clock   clock.Clock
	// PublicURL is the configured public origin (scheme://host[:port]); cookie mutations must come from it.
	PublicURL string
	// SecureCookies sets Secure and the __Host- prefix (true when PublicURL is https).
	SecureCookies bool
	// TrustedProxies may set X-Forwarded-For; other peers' headers are ignored.
	TrustedProxies []netip.Prefix
	MaxBodyBytes   int64
	// StreamRevalidate bounds how long an open watch stream outlives a revoked session or token (SM-AUTH-007).
	StreamRevalidate time.Duration
	// Extra registers additional operation groups (nodes, spaces, runs…) supplied by other packages.
	Extra []func(api huma.API, d *Deps)
	// ExtraRules are the authorization rules of the Extra operations.
	ExtraRules map[string]Rule
	// Ready reports readiness for the public system info.
	Ready func() bool
}

func (d *Deps) log() *slog.Logger {
	if d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return d.Log
}

func (d *Deps) now() time.Time {
	if d.Clock == nil {
		return time.Now()
	}
	return d.Clock.Now()
}

// Config returns the OpenAPI document configuration.
func Config() huma.Config {
	c := huma.DefaultConfig("SpaceMan API", buildinfo.Get().Version)
	c.Servers = []*huma.Server{{URL: Prefix}}
	c.DocsPath = "" // the docs UI loads third-party scripts, which the CSP forbids; the site renders the spec instead
	c.OpenAPIPath = "/openapi"
	c.Info.Description = "Administrative API for SpaceMan. Browser sessions use a session cookie and send the session's CSRF token in " +
		"X-CSRF-Token on state-changing requests. Automation sends an API token (`Authorization: Bearer spaceman_at_…`). " +
		"Updates require If-Match with the resource version; idempotent creates accept an Idempotency-Key header."
	c.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"bearer":  {Type: "http", Scheme: "bearer", Description: "API token (spaceman_at_…) or the bootstrap root token (spaceman_rt_…)"},
		"session": {Type: "apiKey", In: "cookie", Name: SessionCookieName(true), Description: "Web UI session cookie (sm_session on plain-http development)"},
	}
	c.Security = []map[string][]string{{"bearer": {}}, {"session": {}}}
	return c
}

// Register builds the API on mux and returns it (also used by `spaceman openapi` with a zero Deps).
func Register(mux *http.ServeMux, d *Deps) huma.API {
	api := humago.NewWithPrefix(mux, Prefix, Config())
	api.UseMiddleware(d.authorizeOps(api))
	registerSystem(api, d)
	registerAuth(api, d)
	registerUsers(api, d)
	registerTokens(api, d)
	registerAudit(api, d)
	for _, f := range d.Extra {
		f(api, d)
	}
	return api
}

// Handler returns the complete API handler with its middleware: security headers, request IDs, client IP, body
// limit, authentication and the cross-site check. The watch stream and the OpenAPI document are served on the same mux.
func (d *Deps) Handler() http.Handler {
	mux := http.NewServeMux()
	Register(mux, d)
	mux.Handle("GET "+Prefix+"/watch", d.watchHandler())
	var h http.Handler = mux
	h = d.authenticate(h)
	h = d.limitBody(h)
	h = d.withRequest(h)
	h = securityHeaders(h)
	return d.Metrics.HTTPMiddleware(h)
}
