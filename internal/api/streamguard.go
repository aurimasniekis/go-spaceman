package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
)

// Stream end reasons sent in the final "end" event of a stream closed by the stream guard (SM-AUTH-007, ADR-0033).
const (
	// EndUnauthenticated: the stream's session or token is no longer valid (revoked, expired, idle, user disabled).
	EndUnauthenticated = "unauthenticated"
	// EndForbidden: the credential is valid but no longer grants this stream (a grant was removed or reduced).
	EndForbidden = "forbidden"
)

// DefaultStreamRevalidate is the periodic re-authorization interval of open streams when none is configured.
const DefaultStreamRevalidate = 30 * time.Second

// streamGuard keeps an open stream's authorization current. It re-authorizes the stream immediately when a committed
// revocation may affect it (auth.StreamWatch) and at least once every StreamRevalidate as the backstop for expiry,
// idle timeout and revocations made outside this process. One re-check is a principal lookup (a handful of indexed
// reads) plus an in-memory Casbin check; it never marks the session active, so an open stream cannot keep an idle
// session alive.
type streamGuard struct {
	d     *Deps
	r     *http.Request
	p     *auth.Principal
	allow func(*auth.Principal) bool
	watch *auth.StreamWatch
	tick  *time.Ticker
}

// guardStream starts guarding a stream authenticated as p. allow is the stream's own authorization (nil: being
// authenticated is enough, for streams that filter each item themselves). Call close when the stream ends.
func (d *Deps) guardStream(r *http.Request, p *auth.Principal, allow func(*auth.Principal) bool) *streamGuard {
	g := &streamGuard{d: d, r: r, p: p, allow: allow, tick: time.NewTicker(d.streamRevalidate())}
	if d.Auth != nil {
		g.watch = d.Auth.Revocations().Watch(p)
	}
	return g
}

func (d *Deps) streamRevalidate() time.Duration {
	if d.StreamRevalidate <= 0 {
		return DefaultStreamRevalidate
	}
	return d.StreamRevalidate
}

// revoked delivers a signal when a revocation that may affect the stream committed (nil channel without auth).
func (g *streamGuard) revoked() <-chan struct{} {
	if g.watch == nil {
		return nil
	}
	return g.watch.C
}

// due delivers the periodic re-check.
func (g *streamGuard) due() <-chan time.Time { return g.tick.C }

// check re-authorizes the stream. It returns "" while the stream may continue (and refreshes the principal), or the
// reason to end it.
func (g *streamGuard) check(ctx context.Context) string {
	np, ok := g.d.revalidate(ctx, g.r, g.p)
	if !ok {
		return EndUnauthenticated
	}
	if g.allow != nil && !g.allow(np) {
		return EndForbidden
	}
	g.p = np
	return ""
}

// principal is the stream's current principal.
func (g *streamGuard) principal() *auth.Principal { return g.p }

func (g *streamGuard) close() {
	g.tick.Stop()
	if g.watch != nil {
		g.watch.Close()
	}
}

// endStream writes the final "end" event with the reason the guard closed the stream. Clients must not reconnect
// blindly: they re-check their session first (unauthenticated) or report that access was lost (forbidden).
func endStream(w http.ResponseWriter, fl http.Flusher, reason string) {
	_, _ = fmt.Fprintf(w, "event: end\ndata: {\"reason\":%q}\n\n", reason)
	fl.Flush()
}
