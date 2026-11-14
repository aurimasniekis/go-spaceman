package botapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/tgtest"
)

// assertNoToken fails if any rendering of err contains the token or its secret half.
func assertNoToken(t *testing.T, what string, err error, token string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error", what)
	}
	_, secret, _ := strings.Cut(token, ":")
	for _, s := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(s, token) || strings.Contains(s, secret) {
			t.Errorf("%s: error contains the token: %s", what, s)
		}
	}
}

// TestTokenNeverInErrors drives every error path of the client and checks that no error text contains the token. The
// net/http errors for a request carry its URL, which holds the token, so the transport paths matter most.
func TestTokenNeverInErrors(t *testing.T) {
	fake := tgtest.New(t, tgtest.Options{Limits: tgtest.Limits{Disabled: true}})
	token := fake.Token()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	newClient := func(base string) *botapi.Client {
		c, err := botapi.New(botapi.Options{BaseURL: base, Token: token, Logger: logger})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ctx := context.Background()
	c := newClient(fake.URL())

	// API refusals, including a description that echoes the token (a proxy or a future Telegram message might).
	_, err := c.SendMessage(ctx, botapi.SendMessageParams{ChatID: userChat, Text: strings.Repeat("a", 5000)})
	assertNoToken(t, "too long", err, token)
	fake.FailNext("getMe", 400, "Bad Request: bot"+token+" is odd", 0)
	_, err = c.GetMe(ctx)
	assertNoToken(t, "echoed token", err, token)
	_, secret, _ := strings.Cut(token, ":")
	fake.FailNext("getMe", 400, "Bad Request: secret "+secret, 0)
	_, err = c.GetMe(ctx)
	assertNoToken(t, "echoed secret half", err, token)
	_, err = c.DownloadFile(ctx, "documents/none.bin", io.Discard)
	assertNoToken(t, "download 404", err, token)

	// setWebhook's secret_token is redacted from its error too.
	fake.FailNext("setWebhook", 400, "Bad Request: secret whsec_abcdef is wrong", 0)
	err = c.SetWebhook(ctx, botapi.SetWebhookParams{URL: "https://h.example.com/x", SecretToken: "whsec_abcdef"})
	if err == nil || strings.Contains(err.Error(), "whsec_abcdef") {
		t.Errorf("setWebhook error leaks the secret: %v", err)
	}

	// Connection refused: net/http's *url.Error would print the full URL.
	ln, lerr := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	closedURL := "http://" + ln.Addr().String()
	_ = ln.Close()
	_, err = newClient(closedURL).GetMe(ctx)
	assertNoToken(t, "connection refused", err, token)
	_, err = newClient(closedURL).DownloadFile(ctx, "documents/a.bin", io.Discard)
	assertNoToken(t, "download connection refused", err, token)

	// Cancellation and deadlines stay recognisable without the URL.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = c.GetMe(cctx)
	assertNoToken(t, "canceled", err, token)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: errors.Is(context.Canceled) = false for %v", err)
	}
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer slow.Close()
	dctx, dcancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer dcancel()
	_, err = newClient(slow.URL).GetMe(dctx)
	assertNoToken(t, "deadline", err, token)
	var te *botapi.TransportError
	if !errors.As(err, &te) || !te.Timeout() || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("deadline: %#v", err)
	}

	// A non-Telegram answer (an HTML error page that quotes the request path) is never echoed.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, "<html>bad gateway for %s</html>", r.URL.Path)
	}))
	defer echo.Close()
	_, err = newClient(echo.URL).GetMe(ctx)
	assertNoToken(t, "html answer", err, token)

	// A redirect answer, and a result that doesn't decode.
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1"+r.URL.Path, http.StatusFound)
	}))
	defer redir.Close()
	_, err = newClient(redir.URL).GetMe(ctx)
	assertNoToken(t, "redirect", err, token)
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":"%s"}`, r.URL.Path)
	}))
	defer odd.Close()
	_, err = newClient(odd.URL).GetMe(ctx)
	assertNoToken(t, "undecodable result", err, token)

	// The debug log names methods only.
	if logs.Len() == 0 || !strings.Contains(logs.String(), "method=getMe") {
		t.Fatalf("expected debug log lines, got %q", logs.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), secret) {
		t.Errorf("log contains the token:\n%s", logs.String())
	}
}
