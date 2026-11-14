package app_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/api"
	"github.com/aurimasniekis/go-spaceman/internal/app"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

const guardIdle = time.Second

// servePublic serves central's real public server (handler and timeouts) on a fresh loopback port.
func servePublic(t *testing.T, h *harness) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := app.PublicServer(h.c, context.Background())
	srv.ErrorLog = log.New(io.Discard, "", 0)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func guardedHarness(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarnessWith(t, func(c *config.Config) {
		c.HTTP.ReadIdleTimeout = config.Duration(guardIdle)
		c.HTTP.IdleTimeout = config.Duration(guardIdle)
	})
	return h, servePublic(t, h)
}

// TG-3a follow-up: a client that sends request headers and then a slow, partial or no body can't hold a connection on
// central's public listener, on any route: UI, API refusals, a handler that reads the body, the agent endpoint without an
// upgrade, and a kept-alive connection that never sends its next request. Before the guard every probe held its
// connection (and a goroutine) for as long as the client kept it open, with no response.
func TestSlowOrEmptyBodyCannotHoldConnection(t *testing.T) {
	t.Parallel()
	h, addr := guardedHarness(t)
	// The configured origin, so mutations get past the cross-site check to authentication and to the sign-in handler.
	origin := "Origin: " + h.srv.URL + "\r\n"
	for _, p := range []struct{ name, req, body, status string }{
		{"ui post, body never sent", "POST / HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n", "", "200"},
		{"api refusal, body never sent", "POST /api/v1/spaces HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" + origin + "Content-Length: 1000\r\n\r\n", "", "401"},
		{"api refusal, chunked body never sent", "POST /api/v1/spaces HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" + origin + "Transfer-Encoding: chunked\r\n\r\n", "", "401"},
		{"sign-in reading a stalled body", "POST /api/v1/auth/login HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n" + origin + "Content-Length: 1000\r\n\r\n", `{"email":`, ""},
		{"agent endpoint without upgrade", "POST /agent/ HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n", "", ""},
		{"kept-alive connection gone quiet", "GET /api/v1/system/info HTTP/1.1\r\nHost: x\r\n\r\n", "", "200"},
	} {
		t.Run(p.name, func(t *testing.T) {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			start := time.Now()
			if _, err := io.WriteString(c, p.req+p.body); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(guardIdle + 4*time.Second))
			b, err := io.ReadAll(c)
			if err != nil && !strings.Contains(err.Error(), "connection reset") {
				t.Fatalf("central still holds the connection (read %q): %v", b, err)
			}
			if p.status != "" && !strings.HasPrefix(string(b), "HTTP/1.1 "+p.status) {
				t.Fatalf("response %q", b)
			}
			t.Logf("connection ended after %v", time.Since(start).Round(time.Millisecond))
		})
	}
}

// The guard doesn't cut what must keep going: an event stream outlives several idle bounds and still delivers changes,
// and a large attachment upload trickled over several idle bounds succeeds.
func TestBodyGuardKeepsStreamsAndUploads(t *testing.T) {
	t.Parallel()
	h, addr := guardedHarness(t)
	ctx := context.Background()
	rootSecret, _, err := h.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Type: audit.ActorUser, ID: "usr_test"}
	en, err := h.c.Fleet.CreateNode(ctx, actor, fleet.CreateNodeInput{Name: "node-a"})
	if err != nil {
		t.Fatal(err)
	}
	space, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "alpha", Name: "Alpha", NodeID: en.Node.ID})
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr + api.Prefix

	t.Run("event stream", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, base+"/watch?live=1", nil)
		req.Header.Set("Authorization", "Bearer "+rootSecret)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("watch: %d", res.StatusCode)
		}
		events := make(chan string, 64)
		go func() {
			sc := bufio.NewScanner(res.Body)
			for sc.Scan() {
				events <- sc.Text()
			}
			close(events)
		}()
		time.Sleep(3 * guardIdle)
		if _, err := h.c.Fleet.CreateSpace(ctx, actor, fleet.CreateSpaceInput{Slug: "bravo", Name: "Bravo", NodeID: en.Node.ID}); err != nil {
			t.Fatal(err)
		}
		deadline := time.After(10 * time.Second)
		for {
			select {
			case line, ok := <-events:
				if !ok {
					t.Fatal("the event stream ended")
				}
				if line == "event: change" {
					return
				}
			case <-deadline:
				t.Fatal("no change event after the idle bound")
			}
		}
	})

	t.Run("slow large upload", func(t *testing.T) {
		const size, piece = 2 << 20, 64 << 10
		gap := guardIdle / 6
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			fw, err := mw.CreateFormFile("file", "big.bin")
			if err == nil {
				buf := make([]byte, piece)
				for sent := 0; sent < size && err == nil; sent += piece {
					time.Sleep(gap)
					_, err = fw.Write(buf)
				}
			}
			if err == nil {
				err = mw.Close()
			}
			_ = pw.CloseWithError(err)
		}()
		req, _ := http.NewRequest(http.MethodPost, base+"/spaces/"+space.ID+"/attachments", pr)
		req.Header.Set("Authorization", "Bearer "+rootSecret)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		start := time.Now()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("upload: %d %s", res.StatusCode, b)
		}
		if took := time.Since(start); took < 3*guardIdle {
			t.Fatalf("the upload took only %v; it must span several idle bounds", took)
		}
		if !strings.Contains(string(b), fmt.Sprint(size)) {
			t.Fatalf("upload response %s", b)
		}
	})
}
