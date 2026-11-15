package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/auth"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

// syncRecorder is a flushing response writer that can be read while the handler still writes.
type syncRecorder struct {
	mu  sync.Mutex
	hdr http.Header
	buf bytes.Buffer
}

func (s *syncRecorder) Header() http.Header { return s.hdr }
func (s *syncRecorder) WriteHeader(int)     {}
func (s *syncRecorder) Flush()              {}
func (s *syncRecorder) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(b)
}
func (s *syncRecorder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// SM-API-002: an idle change-feed stream sends a heartbeat at least every StreamHeartbeat, whichever client
// (browser, CLI, Mini App) holds it, so a proxy that drops idle connections (about 100 s) never sees it idle. The
// clock is fake: testing/synctest advances time only when every goroutine in the bubble is blocked.
func TestWatchHeartbeatWhenIdle(t *testing.T) {
	for name, tc := range map[string]struct {
		set  time.Duration
		want time.Duration
	}{
		"default":    {0, DefaultStreamHeartbeat},
		"configured": {7 * time.Second, 7 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if tc.want >= 60*time.Second {
				t.Fatalf("interval %v is not under 60s", tc.want)
			}
			synctest.Test(t, func(t *testing.T) {
				d := &Deps{Store: storetest.Open(t), StreamHeartbeat: tc.set}
				p := &auth.Principal{User: &auth.User{ID: "usr_1"}}
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), principalKey{}, p))
				w := &syncRecorder{hdr: http.Header{}}
				done := make(chan struct{})
				go func() {
					defer close(done)
					d.watchHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, Prefix+"/watch?live=1", nil).WithContext(ctx))
				}()
				synctest.Wait()
				if strings.Contains(w.String(), "event: heartbeat") {
					t.Fatal("heartbeat before the interval")
				}
				time.Sleep(tc.want - time.Millisecond)
				synctest.Wait()
				if strings.Contains(w.String(), "event: heartbeat") {
					t.Fatal("heartbeat before the interval")
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if !strings.Contains(w.String(), "event: heartbeat\ndata: {}\n\n") {
					t.Fatalf("no heartbeat after %v: %q", tc.want, w.String())
				}
				cancel()
				<-done
			})
		})
	}
}
