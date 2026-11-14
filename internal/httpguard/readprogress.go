package httpguard

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultBodyBound is the total time a request body may take when the handler sets no read deadline of its own (huma's
// BodyReadTimeout default is the same 5 s). It starts at the body's first read. A handler raises or clears it through
// http.ResponseController.SetReadDeadline, as huma does for the attachment upload.
var DefaultBodyBound = 5 * time.Second

// ReadProgress wraps next so a request body must make progress: every read of the body gets idle to bring data, so a
// client that declares a body and then sends nothing (or stops half-way) is cut off instead of holding a goroutine and
// its connection. Progress alone is not a total bound, so a deadline the handler sets (huma's BodyReadTimeout, 5 s
// unless an operation says otherwise) is honoured as well: each read waits for the sooner of now+idle and that
// deadline. A large upload on an operation with a long bound that keeps moving is therefore never cut off early, and
// a trickle can't outlive the operation's bound. Once the body has been read to its end the deadline is cleared, so a
// handler that goes on to stream a response is unaffected; requests without a body (Server-Sent Events, downloads,
// WebSocket handshakes) get no deadline at all. Upgrade headers alone exempt nothing: a request that carries a body is
// guarded whatever its headers say.
//
// A handler that responds without reading the whole body (a refusal: 401, 404, 405, 413) would otherwise make net/http
// drain up to 256 KiB of it with no deadline, before the response to keep the connection and again after the handler
// returns to find the body's end. Over HTTP/1 the guard therefore marks such a response Connection: close before its
// header is written, and sets a read deadline of now when the handler returns, so neither drain waits. HTTP/2 needs
// neither: an unread stream is reset, not drained.
func ReadProgress(next http.Handler, idle time.Duration) http.Handler {
	if idle <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body == nil || r.Body == http.NoBody {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		b := &progressBody{ReadCloser: r.Body, rc: rc, idle: idle}
		r.Body = b
		// HTTP/2 has no drain to stop, Connection is not an HTTP/2 header, and a read deadline of now would reset the
		// stream, so the HTTP/1 handling (h1) is kept off it; the deadline bookkeeping applies to both.
		h1 := r.ProtoMajor == 1
		next.ServeHTTP(&unreadGuardWriter{ResponseWriter: w, body: b, h1: h1}, r)
		if h1 && !b.eof.Load() {
			// The response is complete; whatever remains of the body is not worth waiting for. net/http's drain fails at
			// once, so it closes the connection (and marks a header it has yet to write Connection: close).
			_ = rc.SetReadDeadline(time.Now())
		}
	})
}

// progressBody gives each read of a request body its own deadline and clears it at the body's end.
type progressBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
	eof  atomic.Bool

	mu     sync.Mutex
	cap    time.Time // the total bound: the handler's own deadline, or the default one started at the first read
	capSet bool      // the handler set (or cleared) the bound itself, so the default no longer applies
}

// setCap records the deadline the handler asked for and applies it at once.
func (b *progressBody) setCap(t time.Time) error {
	b.mu.Lock()
	b.cap = t
	b.capSet = true
	b.mu.Unlock()
	if t.IsZero() {
		return nil
	}
	return b.rc.SetReadDeadline(t)
}

// next is the deadline for one more read: now+idle, but never later than the handler's own.
func (b *progressBody) next() time.Time {
	now := time.Now()
	d := now.Add(b.idle)
	b.mu.Lock()
	if !b.capSet && b.cap.IsZero() {
		// The handler has set no deadline (routes outside huma: enrolment, the MCP endpoint): the body gets the default
		// total from its first read, so a client trickling inside the idle bound can't hold the request.
		b.cap = now.Add(DefaultBodyBound)
	}
	c := b.cap
	b.mu.Unlock()
	if !c.IsZero() && c.Before(d) {
		return c
	}
	return d
}

func (b *progressBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(b.next())
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		// No deadline after the end: net/http now reads the connection in the background to notice a client that goes
		// away, and a deadline firing there would cancel the request's context. net/http clears the deadline when that
		// read starts, but a read past the end (a decoder checking for more) would set it again.
		b.eof.Store(true)
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// unreadGuardWriter marks a response Connection: close when its header is written before the body was read to the end,
// which stops net/http from draining the rest of the body (with no deadline) before it sends the header.
type unreadGuardWriter struct {
	http.ResponseWriter
	body *progressBody
	h1   bool
}

// SetReadDeadline is how huma applies an operation's BodyReadTimeout: it is kept as the total bound for the body (the
// guard's per-read deadline never goes past it) instead of being overwritten by the next read.
func (w *unreadGuardWriter) SetReadDeadline(t time.Time) error { return w.body.setCap(t) }

// commit marks the response Connection: close if its final header is being written with the body unread. Interim
// responses (1xx, a 101 included: its Connection header says Upgrade) are left alone.
func (w *unreadGuardWriter) commit(code int) {
	if !w.h1 || (code >= 100 && code < 200) {
		return
	}
	if !w.body.eof.Load() {
		w.Header().Set("Connection", "close")
	}
}

func (w *unreadGuardWriter) WriteHeader(code int) {
	w.commit(code)
	w.ResponseWriter.WriteHeader(code)
}

func (w *unreadGuardWriter) Write(p []byte) (int, error) {
	w.commit(http.StatusOK)
	return w.ResponseWriter.Write(p)
}

// Flush commits the header like Write does (Server-Sent Events flush after every event).
func (w *unreadGuardWriter) Flush() { _ = w.FlushError() }

// FlushError is Flush that reports the error (http.ResponseController prefers it).
func (w *unreadGuardWriter) FlushError() error {
	w.commit(http.StatusOK)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap returns the wrapped writer for http.ResponseController.
func (w *unreadGuardWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
