package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

type (
	principalKey    struct{}
	sessionTokenKey struct{}
	clientIPKey     struct{}
)

// PrincipalFrom returns the authenticated principal of a request (nil when anonymous).
func PrincipalFrom(ctx context.Context) *auth.Principal {
	p, _ := ctx.Value(principalKey{}).(*auth.Principal)
	return p
}

func sessionTokenFrom(ctx context.Context) string {
	t, _ := ctx.Value(sessionTokenKey{}).(string)
	return t
}

// ClientIP returns the request's client address (after trusted-proxy processing).
func ClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

// SessionCookieName is the web session cookie: __Host-sm_session with Secure, sm_session on plain-http development.
func SessionCookieName(secure bool) string {
	if secure {
		return "__Host-sm_session"
	}
	return "sm_session"
}

const (
	bearerPrefix = "Bearer "
	// CSRFHeader carries the session's CSRF token on state-changing requests.
	CSRFHeader = "X-CSRF-Token"
	// RequestIDHeader echoes the request's correlation ID.
	RequestIDHeader = "X-Request-Id"
)

// securityHeaders sets the strict headers every API response carries (copied from Hallpass's policy).
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// withRequest assigns a request ID, resolves the client IP and attaches both to the context (logs, audit).
func (d *Deps) withRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b [12]byte
		_, _ = rand.Read(b[:])
		id := "req_" + hex.EncodeToString(b[:])
		ip := d.clientIP(r)
		ctx := context.WithValue(r.Context(), clientIPKey{}, ip)
		ctx = obs.WithIDs(ctx, obs.HTTPRequestID(id))
		ctx = audit.WithMeta(ctx, audit.Meta{SourceIP: ip, CorrelationID: id})
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// clientIP is the TCP peer, or the rightmost X-Forwarded-For entry not in a trusted proxy range when the peer is a
// trusted proxy.
func (d *Deps) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if !d.trusted(peer) {
		return peer.Unmap().String()
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		if !d.trusted(a) {
			return a.Unmap().String()
		}
	}
	return peer.Unmap().String()
}

func (d *Deps) trusted(a netip.Addr) bool {
	for _, p := range d.TrustedProxies {
		if p.Contains(a.Unmap()) {
			return true
		}
	}
	return false
}

// limitBody caps request bodies (uploads use their own, larger limit on their own route).
func (d *Deps) limitBody(next http.Handler) http.Handler {
	limit := d.MaxBodyBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	upload := d.MaxUploadBytes
	if upload <= 0 {
		upload = 25 << 20
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			l := limit
			if strings.HasSuffix(r.URL.Path, "/attachments") {
				l = upload + 64<<10 // the file plus multipart framing; the service enforces the exact file limit
			}
			r.Body = http.MaxBytesReader(w, r.Body, l)
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate resolves a bearer token (API token or root token) or the session cookie into a principal, and refuses
// cross-site state-changing requests: a mutation carrying a session cookie, or an unauthenticated mutation (sign-in),
// must come from the configured public origin. Bearer requests never fall back to the cookie.
func (d *Deps) authenticate(next http.Handler) http.Handler {
	authLimit := newIPLimiter(rate.Limit(20.0/60), 20) // sign-in attempts per address: 20 per minute (burst 20)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ip := ClientIP(ctx)
		var p *auth.Principal
		authz := r.Header.Get("Authorization")
		bearer := strings.HasPrefix(authz, bearerPrefix)
		if bearer && d.Auth != nil {
			tok := strings.TrimPrefix(authz, bearerPrefix)
			var err error
			switch {
			case strings.HasPrefix(tok, auth.RootTokenPrefix):
				p, err = d.Auth.AuthenticateRootToken(ctx, tok, ip)
			case strings.HasPrefix(tok, auth.APITokenPrefix):
				p, err = d.Auth.AuthenticateAPIToken(ctx, tok, ip)
			default:
				err = auth.ErrUnauthorized
			}
			if err != nil {
				if !authLimit.allow(ip) {
					writeProblem(w, problemFor(errRateLimited))
					return
				}
				if errors.Is(err, auth.ErrUnauthorized) {
					writeProblem(w, errUnauthenticated())
				} else {
					var pr *Problem
					if !errors.As(d.fail(ctx, "authenticate", err), &pr) {
						pr = newProblem(http.StatusInternalServerError, "internal_error", "internal error", nil)
					}
					writeProblem(w, pr)
				}
				return
			}
		} else if ck, err := r.Cookie(SessionCookieName(d.SecureCookies)); err == nil && ck.Value != "" && d.Auth != nil {
			ctx = context.WithValue(ctx, sessionTokenKey{}, ck.Value)
			if pp, err := d.Auth.Authenticate(ctx, ck.Value); err == nil {
				p = pp
			}
		}
		if mutating(r.Method) && !bearer && !d.sameOrigin(r) {
			writeProblem(w, errCrossSite())
			return
		}
		if p == nil && mutating(r.Method) && strings.HasPrefix(r.URL.Path, Prefix+"/auth/") && !authLimit.allow(ip) {
			writeProblem(w, problemFor(errRateLimited))
			return
		}
		if p != nil {
			ids := []obs.CorrelationID{}
			if p.User != nil {
				ids = append(ids, obs.UserID(p.User.ID))
			}
			if p.TokenID != "" {
				ids = append(ids, obs.TokenID(p.TokenID))
			}
			ctx = context.WithValue(obs.WithIDs(ctx, ids...), principalKey{}, p)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func mutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// sameOrigin accepts Origin == public URL origin, or (when Origin is absent) Sec-Fetch-Site: same-origin.
func (d *Deps) sameOrigin(r *http.Request) bool {
	want := ""
	if u, err := url.Parse(d.PublicURL); err == nil && u.Host != "" {
		want = u.Scheme + "://" + u.Host
	}
	if o := r.Header.Get("Origin"); o != "" {
		return want != "" && strings.EqualFold(o, want)
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// ipLimiter is a per-address token bucket (IPv6 grouped by /64), pruned lazily.
type ipLimiter struct {
	mu      sync.Mutex
	lim     rate.Limit
	burst   int
	buckets map[string]*ipBucket
	lastGC  time.Time
}

type ipBucket struct {
	l    *rate.Limiter
	seen time.Time
}

func newIPLimiter(lim rate.Limit, burst int) *ipLimiter {
	return &ipLimiter{lim: lim, burst: burst, buckets: map[string]*ipBucket{}}
}

func (l *ipLimiter) allow(ip string) bool {
	key := ip
	if a, err := netip.ParseAddr(ip); err == nil && a.Is6() && !a.Is4In6() {
		pfx, _ := a.Prefix(64)
		key = pfx.String()
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 30*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b := l.buckets[key]
	if b == nil {
		b = &ipBucket{l: rate.NewLimiter(l.lim, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.l.Allow()
}
