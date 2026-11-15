package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// wsSink connects a to a local websocket server and returns the run ids of the run.report messages it receives.
func wsSink(t *testing.T, a *Agent) (conn *websocket.Conn, reports chan string) {
	t.Helper()
	reports = make(chan string, 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		for {
			_, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			env, err := proto.Decode(b)
			if err != nil || env.Type != proto.TypeRunReport {
				continue
			}
			var rr proto.RunReport
			if env.Decode(&rr) == nil {
				reports <- rr.RunID
			}
		}
	}))
	t.Cleanup(srv.Close)
	u := "ws" + srv.URL[len("http"):]
	conn, _, err := websocket.Dial(context.Background(), u, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	a.mu.Lock()
	a.conn = conn
	a.mu.Unlock()
	return conn, reports
}

func expectReport(t *testing.T, reports chan string, id string) {
	t.Helper()
	select {
	case got := <-reports:
		if got != id {
			t.Fatalf("reported %s, want %s", got, id)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("run %s was not reported", id)
	}
}

// After a central restore the node counted a run as acknowledged, but central lists it as active again: the final state
// is reported at once (not at the next hello, after the stale sweeper ended the run lost) and is unacknowledged again.
func TestReviewRestoreAckedRunNeverResent(t *testing.T) {
	a := pruneAgent(t, t.TempDir())
	_, reports := wsSink(t, a)
	seedRun(t, a, "run_x", proto.RunExited, 2*day)
	r, _ := a.j.getRun(context.Background(), "run_x")
	a.noteSent([]runRow{r})
	ack(a) // central held the ended state
	if hello, follow, _ := a.helloRuns(context.Background()); len(hello)+len(follow) != 0 {
		t.Fatalf("acknowledged run still due: %d %d", len(hello), len(follow))
	}
	ack(a, "run_x") // central was restored from an older backup: it lists the run as active again
	expectReport(t, reports, "run_x")
	if _, follow, _ := a.helloRuns(context.Background()); len(follow) != 1 {
		t.Fatalf("run is not due again for the next connection: %d", len(follow))
	}
}

// A follow-up report that was written but never applied is reported again when central still lists the run.
func TestReviewFollowUpLostNeverResent(t *testing.T) {
	a := pruneAgent(t, t.TempDir())
	_, reports := wsSink(t, a)
	seedRun(t, a, "run_old", proto.RunExited, 3*day)
	_, follow, _ := a.helloRuns(context.Background())
	a.noteSent(follow) // written to the connection, which dropped before central applied it
	ack(a, "run_old")
	expectReport(t, reports, "run_old")
	if _, follow, _ = a.helloRuns(context.Background()); !slices.Equal(runRowIDs(follow), []string{"run_old"}) {
		t.Fatalf("follow-up %v", runRowIDs(follow))
	}
}

func runRowIDs(rs []runRow) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.RunID)
	}
	return out
}
