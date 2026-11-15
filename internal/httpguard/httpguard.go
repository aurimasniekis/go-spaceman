// Package httpguard bounds what a client that stops reading can cost central.
package httpguard

import (
	"net/http"
	"strings"
	"time"
)

// chunk is the most a single network write gets under one deadline.
const chunk = 32 << 10

// WriteProgress wraps next so every response write must make progress: each piece of at most 32 KiB gets idle to
// reach the client's connection, and a write that can't (the client stopped reading) fails, so the handler ends and the
// connection is closed instead of holding the response in memory. It bounds progress, not the total time, so slow
// transfers that keep moving are unaffected. A WebSocket handshake (see websocketHandshake) is passed through untouched: it
// hijacks the connection and keeps its own timeouts. Upgrade headers on anything else get no bypass.
func WriteProgress(next http.Handler, idle time.Duration) http.Handler {
	if idle <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if websocketHandshake(r) {
			next.ServeHTTP(w, r)
			return
		}
		rc := http.NewResponseController(w)
		// A deadline a previous response on this keep-alive connection left set must not cut this one short.
		_ = rc.SetWriteDeadline(time.Time{})
		pw := &progressWriter{ResponseWriter: w, rc: rc, idle: idle}
		next.ServeHTTP(pw, r)
		// net/http flushes the rest of the response after the handler returns: that, too, must make progress.
		_ = rc.SetWriteDeadline(time.Now().Add(idle))
	})
}

// websocketHandshake reports whether r is a real WebSocket opening handshake: a body-less HTTP/1.1 GET with the upgrade
// headers (RFC 6455 §4.1). Headers alone are not enough: any client can add them to a POST that declares a body.
func websocketHandshake(r *http.Request) bool {
	return r.Method == http.MethodGet && r.ProtoMajor == 1 && r.ProtoMinor >= 1 &&
		(r.Body == nil || r.Body == http.NoBody) && r.ContentLength <= 0 && len(r.TransferEncoding) == 0 &&
		websocketUpgrade(r)
}

// websocketUpgrade reports whether r asks for a WebSocket: Connection has the upgrade token and Upgrade the websocket
// one (both case-insensitive, RFC 6455 §4.2.1).
func websocketUpgrade(r *http.Request) bool {
	return hasToken(r.Header.Values("Connection"), "upgrade") && hasToken(r.Header.Values("Upgrade"), "websocket")
}

// hasToken reports whether a comma-separated header (every value of it) lists token, ignoring case.
func hasToken(values []string, token string) bool {
	for _, v := range values {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

type progressWriter struct {
	http.ResponseWriter
	rc   *http.ResponseController
	idle time.Duration
}

func (w *progressWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := min(len(p), chunk)
		_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
		m, err := w.ResponseWriter.Write(p[:c])
		n += m
		if err != nil {
			return n, err
		}
		p = p[c:]
	}
	return n, nil
}

// Flush flushes under the same deadline (Server-Sent Events flush after every event).
func (w *progressWriter) Flush() { _ = w.FlushError() }

// FlushError is Flush that reports the error (http.ResponseController prefers it).
func (w *progressWriter) FlushError() error {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
	return w.rc.Flush()
}

// Unwrap returns the wrapped writer for http.ResponseController.
func (w *progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
