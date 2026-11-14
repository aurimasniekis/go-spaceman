package httpguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"
)

// readServer serves h behind ReadProgress (inside WriteProgress, as central does) and reports what each handler returned.
func readServer(t *testing.T, readIdle time.Duration, h func(w http.ResponseWriter, r *http.Request) error) (*httptest.Server, chan error) {
	t.Helper()
	done := make(chan error, 8)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { done <- h(w, r) })
	srv := httptest.NewUnstartedServer(WriteProgress(ReadProgress(inner, readIdle), time.Minute))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, done
}

// dial opens a raw TCP connection to srv and sends head (request line and headers, no final blank line) plus body.
func dial(t *testing.T, srv *httptest.Server, head, body string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, head+"\r\n"+body); err != nil {
		t.Fatal(err)
	}
	return c
}

// awaitClose reads c until the server closes it, failing if that takes longer than within. It returns what was read.
func awaitClose(t *testing.T, c net.Conn, within time.Duration) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	b, err := io.ReadAll(c)
	if err != nil && !errors.Is(err, net.ErrClosed) && !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("the server still holds the connection after %v (read %q): %v", within, b, err)
	}
	return string(b)
}

// A refusal before the body is read (the client declared a body and sends none of it) gets its response and the
// connection closed at once, whatever the read idle bound: net/http must not drain the body before the response, to
// keep the connection, or after it, to find the body's end. The bound here is a minute, so only the refusal path can
// end the connection within the test's few seconds.
func TestRefusalWithUnreadBodyClosesAtOnce(t *testing.T) {
	srv, done := readServer(t, time.Minute, func(w http.ResponseWriter, _ *http.Request) error {
		http.Error(w, "no", http.StatusUnauthorized)
		return nil
	})
	for _, tc := range []struct{ name, headers, body string }{
		{"content-length, nothing sent", "Content-Length: 1000\r\n", ""},
		{"content-length, part sent", "Content-Length: 1000\r\n", "{\"a\":"},
		{"chunked, nothing sent", "Transfer-Encoding: chunked\r\n", ""},
		{"chunked, part sent", "Transfer-Encoding: chunked\r\n", "5\r\nhello\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t, srv, "POST /x HTTP/1.1\r\nHost: x\r\n"+tc.headers, tc.body)
			got := awaitClose(t, c, 3*time.Second)
			if !strings.HasPrefix(got, "HTTP/1.1 401") || !strings.Contains(got, "Connection: close") {
				t.Fatalf("response %q", got)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A refusal whose header the handler writes and flushes itself, after a partial read, also closes at once.
func TestRefusalAfterPartialReadClosesAtOnce(t *testing.T) {
	srv, done := readServer(t, time.Minute, func(w http.ResponseWriter, r *http.Request) error {
		if _, err := io.ReadFull(r.Body, make([]byte, 5)); err != nil {
			return err
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		if _, err := w.Write([]byte("too large")); err != nil {
			return err
		}
		// Flushing sends the header while the handler runs: net/http would drain the body first unless it closes.
		return http.NewResponseController(w).Flush()
	})
	c := dial(t, srv, "POST /x HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n", "hello")
	if got := awaitClose(t, c, 3*time.Second); !strings.HasPrefix(got, "HTTP/1.1 413") {
		t.Fatalf("response %q", got)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A handler that reads a body the client stops sending gets a read error within the idle bound, and the connection
// ends, instead of both waiting for as long as the client keeps it open.
func TestStalledBodyIsCutOff(t *testing.T) {
	srv, done := readServer(t, idle, func(_ http.ResponseWriter, r *http.Request) error {
		_, err := io.ReadAll(r.Body)
		return err
	})
	for _, tc := range []struct{ name, headers, body string }{
		{"content-length", "Content-Length: 1000\r\n", "{\"a\":"},
		{"chunked", "Transfer-Encoding: chunked\r\n", "5\r\nhello\r\n"},
		{"nothing sent", "Content-Length: 10\r\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := dial(t, srv, "POST /x HTTP/1.1\r\nHost: x\r\n"+tc.headers, tc.body)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("reading a stalled body succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the handler is still reading a body the client stopped sending")
			}
			awaitClose(t, c, 3*time.Second)
		})
	}
}

// trickle sends n bytes in pieces of piece bytes, one every gap: a slow upload that keeps moving.
type trickle struct {
	n, piece int
	gap      time.Duration
	sent     int
}

func (tr *trickle) Read(p []byte) (int, error) {
	if tr.sent >= tr.n {
		return 0, io.EOF
	}
	if tr.sent > 0 {
		time.Sleep(tr.gap)
	}
	m := min(len(p), tr.piece, tr.n-tr.sent)
	for i := range m {
		p[i] = 'x'
	}
	tr.sent += m
	return m, nil
}

// A slow upload that keeps moving is never cut off, though it takes many times the idle bound, with a declared length
// or chunked.
func TestSlowUploadThatKeepsMovingSucceeds(t *testing.T) {
	const n = 64 << 10
	srv, done := readServer(t, idle, func(w http.ResponseWriter, r *http.Request) error {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(w, len(b))
		return err
	})
	for _, chunked := range []bool{false, true} {
		t.Run(fmt.Sprint("chunked=", chunked), func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL, &trickle{n: n, piece: 4 << 10, gap: idle / 3})
			if !chunked {
				req.ContentLength = n
			}
			start := time.Now()
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(res.Body)
			_ = res.Body.Close()
			if err := <-done; err != nil || string(b) != fmt.Sprint(n) {
				t.Fatalf("handler %v, response %q", err, b)
			}
			if took := time.Since(start); took < 4*idle {
				t.Fatalf("the upload took only %v; it must span several idle periods", took)
			}
		})
	}
}

// Once the body is read to its end the deadline is gone: a handler that then streams for many idle periods keeps its
// request context and the client gets every event (net/http's background read must not hit a stale deadline).
func TestStreamAfterBodyIsNotCut(t *testing.T) {
	srv, done := readServer(t, idle, func(w http.ResponseWriter, r *http.Request) error {
		if _, err := io.ReadAll(r.Body); err != nil {
			return err
		}
		// A read past the end (a JSON decoder checking for more) must not set a deadline again.
		if _, err := r.Body.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			return fmt.Errorf("read past the end: %w", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3 {
			if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
				return err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return err
			}
			select {
			case <-r.Context().Done():
				return r.Context().Err()
			case <-time.After(2 * idle):
			}
		}
		return nil
	})
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(`{"follow":true}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err := <-done; err != nil || strings.Count(string(b), "data:") != 3 {
		t.Fatalf("handler %v, stream %q", err, b)
	}
}

// Requests whose body is read to the end, and requests without a body, keep their connection alive; a handler that
// takes longer than the idle bound to answer a GET is not cut (it has no body to wait for).
func TestKeepAliveAfterReadBody(t *testing.T) {
	srv, done := readServer(t, idle, func(w http.ResponseWriter, r *http.Request) error {
		if r.Method == http.MethodGet {
			// No body to read, and the handler doesn't try: the connection must stay alive all the same.
			time.Sleep(2 * idle)
		} else if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return err
		}
		_, err := w.Write([]byte("ok"))
		return err
	})
	cl := srv.Client()
	reused := 0
	for i, method := range []string{http.MethodPost, http.MethodPost, http.MethodGet, http.MethodPost} {
		var body io.Reader
		if method == http.MethodPost {
			body = bytes.NewReader([]byte(`{"n":1}`))
		}
		trace := &httptrace.ClientTrace{GotConn: func(ci httptrace.GotConnInfo) {
			if ci.Reused {
				reused++
			}
		}}
		req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), method, srv.URL, body)
		res, err := cl.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err := <-done; err != nil || string(b) != "ok" || res.Close {
			t.Fatalf("request %d: handler %v, body %q, close %v", i, err, b, res.Close)
		}
	}
	if reused != 3 {
		t.Fatalf("%d of 3 later requests reused the connection", reused)
	}
}

// A WebSocket upgrade's hijacked connection keeps no deadline from the guard: it is still usable after the idle bound.
func TestReadGuardLeavesUpgradesAlone(t *testing.T) {
	srv, done := readServer(t, idle, func(w http.ResponseWriter, _ *http.Request) error {
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return err
		}
		defer conn.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = brw.Flush()
		line, err := brw.ReadString('\n')
		if err != nil {
			return err
		}
		_, _ = brw.WriteString("echo " + line)
		return brw.Flush()
	})
	c := dial(t, srv, "GET /agent/ HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n", "")
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade: %v %v", res, err)
	}
	time.Sleep(3 * idle)
	if _, err := io.WriteString(c, "ping\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil || line != "echo ping\n" {
		t.Fatalf("after the idle bound: %q %v", line, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Over HTTP/2 (central serving TLS itself) a refusal with an unread body still delivers its whole response (the
// HTTP/1 close-and-drain handling must not reset the stream first), and a stalled body is cut off.
func TestReadGuardOverHTTP2(t *testing.T) {
	done := make(chan error, 4)
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			done <- fmt.Errorf("protocol %s", r.Proto)
			return
		}
		if r.URL.Path == "/refuse" {
			http.Error(w, strings.Repeat("no ", 1000), http.StatusUnauthorized)
			done <- nil
			return
		}
		_, err := io.ReadAll(r.Body)
		done <- err
	})
	srv := httptest.NewUnstartedServer(WriteProgress(ReadProgress(inner, idle), time.Minute))
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	cl := srv.Client()

	pr, pw := io.Pipe() // a body the client never sends
	t.Cleanup(func() { _ = pw.Close() })
	res, err := cl.Post(srv.URL+"/refuse", "application/json", pr)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != http.StatusUnauthorized || len(b) != 3001 {
		t.Fatalf("refusal: %d, %d bytes, %v", res.StatusCode, len(b), err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	pr2, pw2 := io.Pipe()
	t.Cleanup(func() { _ = pw2.Close() })
	go func() { _, _ = pw2.Write([]byte(`{"a":`)) }()
	go func() {
		if res, err := cl.Post(srv.URL+"/read", "application/json", pr2); err == nil {
			_ = res.Body.Close()
		}
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("reading a stalled body succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler is still reading a body the client stopped sending")
	}
}

// serveUnit runs h behind ReadProgress on a recorder with a body the handler has not read, the way a refusal sees it.
func serveUnit(t *testing.T, major int, body io.ReadCloser, h http.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Body = body
	req.ProtoMajor = major
	rec := httptest.NewRecorder()
	ReadProgress(h, time.Minute).ServeHTTP(rec, req)
	return rec
}

func unreadBody() io.ReadCloser { return io.NopCloser(strings.NewReader("abc")) }

// Which responses get Connection: close, and when: the header is marked as it is committed (a write, a flush or an
// explicit WriteHeader), never for an interim 1xx or a 101, never for HTTP/2, and not for a nil body.
func TestConnectionCloseMarking(t *testing.T) {
	closeOf := func(w http.ResponseWriter) string { return w.Header().Get("Connection") }
	t.Run("write commits", func(t *testing.T) {
		serveUnit(t, 1, unreadBody(), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("x"))
			if closeOf(w) != "close" {
				t.Error("Write did not mark Connection: close")
			}
		})
	})
	t.Run("flush commits", func(t *testing.T) {
		serveUnit(t, 1, unreadBody(), func(w http.ResponseWriter, _ *http.Request) {
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Error(err)
			}
			if closeOf(w) != "close" {
				t.Error("Flush did not mark Connection: close")
			}
		})
	})
	t.Run("interim 1xx does not commit", func(t *testing.T) {
		serveUnit(t, 1, unreadBody(), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			if closeOf(w) != "" {
				t.Error("an interim response was marked Connection: close")
			}
			w.WriteHeader(http.StatusUnauthorized)
			if closeOf(w) != "close" {
				t.Error("the final response was not marked")
			}
		})
	})
	t.Run("101 is left alone", func(t *testing.T) {
		serveUnit(t, 1, unreadBody(), func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusSwitchingProtocols)
			if closeOf(w) != "" {
				t.Error("a 101 was marked Connection: close")
			}
		})
	})
	t.Run("HTTP/2 is left alone", func(t *testing.T) {
		serveUnit(t, 2, unreadBody(), func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("x"))
			if closeOf(w) != "" {
				t.Error("an HTTP/2 response was marked Connection: close")
			}
		})
	})
	t.Run("nil body is left alone", func(t *testing.T) {
		serveUnit(t, 1, nil, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("x"))
			if closeOf(w) != "" {
				t.Error("a request without a body was marked Connection: close")
			}
		})
	})
}

// A flush the underlying writer cannot do is reported, not swallowed.
func TestFlushErrorIsReported(t *testing.T) {
	type bare struct{ http.ResponseWriter } // hides the recorder's Flush
	req := httptest.NewRequest(http.MethodPost, "/x", unreadBody())
	var got error
	h := ReadProgress(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		got = http.NewResponseController(w).Flush()
	}), time.Minute)
	h.ServeHTTP(bare{httptest.NewRecorder()}, req)
	if !errors.Is(got, http.ErrNotSupported) {
		t.Fatalf("flush error %v, want ErrNotSupported", got)
	}
}

// A zero bound turns the guard off: a body that pauses is still read.
func TestZeroIdleDisablesReadGuard(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%d %v", len(b), err)
	})
	srv := httptest.NewServer(ReadProgress(inner, 0))
	t.Cleanup(srv.Close)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("ab"))
		time.Sleep(3 * idle)
		_, _ = pw.Write([]byte("cd"))
		_ = pw.Close()
	}()
	res, err := http.Post(srv.URL, "text/plain", pr)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(b) != "4 <nil>" {
		t.Fatalf("got %q", b)
	}
}

// Upgrade headers exempt nothing: a request that carries a body is guarded however it is dressed (review: Connection:
// Upgrade + Upgrade: websocket on a POST with a Content-Length let a client hold any route forever).
func TestUpgradeHeadersWithBodyAreStillGuarded(t *testing.T) {
	srv, done := readServer(t, idle, func(_ http.ResponseWriter, r *http.Request) error {
		_, err := io.ReadAll(r.Body)
		return err
	})
	c := dial(t, srv, "POST /agent/ HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nContent-Length: 4\r\n", "ab")
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the stalled body was read to its end")
		}
	case <-time.After(5 * idle):
		t.Fatal("the stalled body of an upgrade-headed POST was never cut off")
	}
	awaitClose(t, c, 2*time.Second)
}

// The deadline a handler sets (huma's BodyReadTimeout) is a total bound: a body trickled inside every idle bound is
// still cut off when it passes, and a bound that is longer than the idle one never extends it.
func TestHandlerDeadlineIsATotalBound(t *testing.T) {
	trickle := func(t *testing.T, srv *httptest.Server, every time.Duration, for_ time.Duration) {
		c := dial(t, srv, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n", "")
		end := time.Now().Add(for_)
		for time.Now().Before(end) {
			time.Sleep(every)
			if _, err := io.WriteString(c, "x"); err != nil {
				return
			}
		}
	}
	t.Run("trickle past the bound", func(t *testing.T) {
		const bound = 6 * idle
		srv, done := readServer(t, 4*idle, func(w http.ResponseWriter, r *http.Request) error {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bound))
			start := time.Now()
			_, err := io.ReadAll(r.Body)
			if took := time.Since(start); err == nil || took < bound-idle || took > bound+3*idle {
				return fmt.Errorf("read ended after %v with %w, want an error near %v", took, err, bound)
			}
			return nil
		})
		go trickle(t, srv, idle/2, 3*bound)
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(3 * bound):
			t.Fatal("the trickled body was never cut off")
		}
	})
	t.Run("longer bound does not extend idle", func(t *testing.T) {
		srv, done := readServer(t, idle, func(w http.ResponseWriter, r *http.Request) error {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute))
			start := time.Now()
			_, err := io.ReadAll(r.Body)
			if took := time.Since(start); err == nil || took > 4*idle {
				return fmt.Errorf("read ended after %v with %w, want an idle cut-off", took, err)
			}
			return nil
		})
		_ = dial(t, srv, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n", "")
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

// A handler that never calls SetReadDeadline (a plain net/http route) still gets the default total bound, counted from
// the first read; one that raises or clears it through the ResponseController is not held to the default.
func TestDefaultBodyBoundApplies(t *testing.T) {
	old := DefaultBodyBound
	DefaultBodyBound = 6 * idle
	t.Cleanup(func() { DefaultBodyBound = old })
	trickle := func(t *testing.T, srv *httptest.Server, for_ time.Duration) {
		c := dial(t, srv, "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n", "")
		end := time.Now().Add(for_)
		for time.Now().Before(end) {
			time.Sleep(idle / 2)
			if _, err := io.WriteString(c, "x"); err != nil {
				return
			}
		}
	}
	for _, p := range []struct {
		name string
		set  func(w http.ResponseWriter)
		cut  bool
	}{
		{"no deadline set: default cap", func(http.ResponseWriter) {}, true},
		{"raised by the handler", func(w http.ResponseWriter) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(20 * idle))
		}, false},
		{"cleared by the handler", func(w http.ResponseWriter) {
			_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		}, false},
	} {
		t.Run(p.name, func(t *testing.T) {
			srv, done := readServer(t, 4*idle, func(w http.ResponseWriter, r *http.Request) error {
				p.set(w)
				start := time.Now()
				_, err := io.ReadAll(r.Body)
				took := time.Since(start)
				if p.cut && (err == nil || took < DefaultBodyBound-idle || took > DefaultBodyBound+3*idle) {
					return fmt.Errorf("read ended after %v with %w, want an error near %v", took, err, DefaultBodyBound)
				}
				if !p.cut && took < DefaultBodyBound+2*idle {
					return fmt.Errorf("read ended after %v (%w), inside the default bound it was meant to escape", took, err)
				}
				return nil
			})
			go trickle(t, srv, 14*idle)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
