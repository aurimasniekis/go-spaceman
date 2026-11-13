package httpguard

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const idle = 300 * time.Millisecond

// server serves h behind WriteProgress and reports what each handler's last write returned.
func server(t *testing.T, h func(w http.ResponseWriter, r *http.Request) error) (*httptest.Server, chan error) {
	t.Helper()
	done := make(chan error, 4)
	srv := httptest.NewUnstartedServer(WriteProgress(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { done <- h(w, r) }), idle))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, done
}

// rawGet sends a GET on a TCP connection (with a small receive buffer when small is set) and returns it, unread.
func rawGet(t *testing.T, srv *httptest.Server, path string, small bool) *net.TCPConn {
	t.Helper()
	c, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc := c.(*net.TCPConn)
	if small {
		_ = tc.SetReadBuffer(4 << 10)
	}
	t.Cleanup(func() { _ = tc.Close() })
	if _, err := fmt.Fprintf(tc, "GET %s HTTP/1.1\r\nHost: x\r\n\r\n", path); err != nil {
		t.Fatal(err)
	}
	return tc
}

// big writes n bytes in one Write, the way an API handler writes an encoded body.
func big(n int) func(w http.ResponseWriter, _ *http.Request) error {
	return func(w http.ResponseWriter, _ *http.Request) error {
		w.Header().Set("Content-Length", fmt.Sprint(n))
		_, err := w.Write(bytes.Repeat([]byte("x"), n))
		return err
	}
}

// A client that never reads its response: the handler's write fails within the idle bound (plus the socket buffers'
// worth) instead of blocking for as long as the client keeps the connection open.
func TestClientThatStopsReadingIsCutOff(t *testing.T) {
	srv, done := server(t, big(64<<20))
	rawGet(t, srv, "/", true)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the write to a client that never reads succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the handler is still blocked writing to a client that never reads")
	}
}

// A slow client that keeps reading gets the whole response, though it takes many times the idle bound.
func TestSlowReaderGetsEverything(t *testing.T) {
	const n = 2 << 20
	srv, done := server(t, big(n))
	c := rawGet(t, srv, "/", false)
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got := 0
	buf := make([]byte, 8<<10)
	for got < n {
		m, err := res.Body.Read(buf)
		got += m
		if err != nil {
			break
		}
		if m > 0 {
			time.Sleep(10 * time.Millisecond) // under 1 MB/s: far slower than the server writes
		}
	}
	if got != n {
		t.Fatalf("read %d of %d bytes", got, n)
	}
	if err := <-done; err != nil {
		t.Fatalf("handler: %v", err)
	}
	if took := time.Since(start); took < 4*idle {
		t.Fatalf("the slow read took only %v; it must span several idle periods", took)
	}
}

// Server-Sent Events: flushed events reach the client, and an idle gap between events longer than the bound doesn't
// end the stream. A keep-alive connection's next response isn't cut short by the previous one's deadline.
func TestStreamsAndKeepAlive(t *testing.T) {
	srv, done := server(t, func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Path == "/plain" {
			_, err := w.Write([]byte("ok"))
			return err
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := range 3 {
			if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
				return err
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return err
			}
			time.Sleep(2 * idle)
		}
		return nil
	})
	res, err := http.Get(srv.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if strings.Count(string(b), "data:") != 3 || <-done != nil {
		t.Fatalf("stream %q", b)
	}
	time.Sleep(2 * idle) // the connection sits idle past the last response's deadline, then serves another
	res, err = http.Get(srv.URL + "/plain")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(res.Body)
	_ = res.Body.Close()
	if string(b) != "ok" || <-done != nil {
		t.Fatalf("plain %q", b)
	}
}

// WebSocket upgrades are left alone: the handler gets the original writer, which can hijack.
func TestUpgradePassesThrough(t *testing.T) {
	srv, done := server(t, func(w http.ResponseWriter, _ *http.Request) error {
		if _, ok := w.(http.Hijacker); !ok {
			return fmt.Errorf("%T can't hijack", w)
		}
		w.WriteHeader(http.StatusNoContent)
		return nil
	})
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	req.Header.Set("Upgrade", "websocket")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
