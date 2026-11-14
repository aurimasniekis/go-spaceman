package httpguard

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// ReadProgress wraps next so a request body must make progress: every read of the body gets idle to bring data, so a
// client that declares a body and then sends nothing (or stops half-way) is cut off instead of holding a goroutine and
// its connection. It bounds progress, not the total time, so a large upload that keeps moving is never cut off. Once
// the body has been read to its end the deadline is cleared, so a handler that goes on to stream a response is
// unaffected; requests without a body (Server-Sent Events, downloads) and WebSocket upgrades get no deadline at all.
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
		if r.Body == nil || r.Body == http.NoBody || websocketUpgrade(r) {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		b := &progressBody{ReadCloser: r.Body, rc: rc, idle: idle}
		r.Body = b
		if r.ProtoMajor != 1 {
			// Defensive: HTTP/2 has no drain to stop, Connection is not an HTTP/2 header, and a read deadline of now
			// would reset the stream, so the HTTP/1 handling is kept off it.
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&unreadGuardWriter{ResponseWriter: w, body: b}, r)
		if !b.eof.Load() {
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
}

func (b *progressBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
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
	body  *progressBody
	wrote bool
}

func (w *unreadGuardWriter) commit(code int) {
	if w.wrote || (code >= 100 && code < 200 && code != http.StatusSwitchingProtocols) {
		return
	}
	w.wrote = true
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
