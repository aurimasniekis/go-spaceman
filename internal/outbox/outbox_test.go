package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics/metricstest"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

type published struct {
	subject, typ string
	data         json.RawMessage
}

type fakePub struct {
	mu    sync.Mutex
	fails int // remaining failures; -1 fails forever
	panic bool
	got   []published
}

func (f *fakePub) Publish(_ context.Context, subject, typ string, data any, _ ...pulse.PublishOption) (*pulse.PubAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panic {
		panic("boom")
	}
	if f.fails != 0 {
		if f.fails > 0 {
			f.fails--
		}
		return nil, errors.New("nats: no responders available for request")
	}
	f.got = append(f.got, published{subject: subject, typ: typ, data: data.(json.RawMessage)})
	return &pulse.PubAck{Stream: "EVENTS"}, nil
}

func (f *fakePub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.got)
}

type env struct {
	ctx context.Context
	s   *store.Store
	clk *clock.Fake
	m   *metrics.Metrics
	o   *Outbox
}

func newEnv(t *testing.T, enabled bool) *env {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	m := metrics.New()
	return &env{ctx: context.Background(), s: s, clk: clk, m: m, o: New(s, Options{Enabled: enabled, Clock: clk, Metrics: m, URL: "nats://127.0.0.1:4222"})}
}

func task(id string) Payload {
	return TaskStatusChanged{Task: Task{SpaceID: "spc_a", TaskID: id, Kind: "ordinary", Priority: "normal"}, From: "ready", To: "running"}
}

// enqueue records n events, each in its own transaction, and returns their IDs.
func (e *env) enqueue(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	for i := range n {
		if err := e.s.W().Tx(e.ctx, func(tx *store.Tx) error {
			id, err := e.o.Enqueue(e.ctx, tx, Event{Payload: task("tsk_" + string(rune('a'+i))), Correlation: "tsk_1", Causation: "req_1"})
			out = append(out, id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func (e *env) row(t *testing.T, id string) db.EventOutbox {
	t.Helper()
	r, err := e.s.R().GetEventOutbox(e.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) stats(t *testing.T) db.EventOutboxStatsRow {
	t.Helper()
	st, err := e.s.R().EventOutboxStats(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var (
	reSubjectToken = regexp.MustCompile(`^[a-z0-9_-]+$`)
	reType         = regexp.MustCompile(`^spaceman\.[a-z]+\.[a-z_]+\.v[0-9]+$`)
)

// The catalog follows the Pulse subject grammar (docs/contracts/pulse.md) and the Porch naming convention, and every
// subject and type is unique.
func TestCatalogNames(t *testing.T) {
	seenS, seenT := map[string]bool{}, map[string]bool{}
	for _, c := range Catalog() {
		if c.Example.eventName() != c.Name {
			t.Errorf("%s: example has name %v", c.Name.Type(), c.Example.eventName())
		}
		if err := c.Name.validate(); err != nil {
			t.Error(err)
		}
		s, ty := c.Name.Subject(), c.Name.Type()
		toks := strings.Split(s, ".")
		if len(toks) < 4 || toks[0] != "evt" || toks[1] != "spaceman" || len(s) > 256 {
			t.Errorf("subject %q", s)
		}
		for _, tok := range toks {
			if !reSubjectToken.MatchString(tok) {
				t.Errorf("subject %q token %q", s, tok)
			}
		}
		if !reType.MatchString(ty) || seenS[s] || seenT[ty] {
			t.Errorf("type %q / subject %q (duplicate or malformed)", ty, s)
		}
		seenS[s], seenT[ty] = true, true
		if c.Example.spaceID() == "" {
			t.Errorf("%s: example without space", ty)
		}
	}
	if got := RunStartedName.Subject() + " " + RunStartedName.Type(); got != "evt.spaceman.fleet.run.started spaceman.fleet.run_started.v1" {
		t.Fatal(got)
	}
	if err := (Name{"Bad", "x", "y", 1}).validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := (Name{"a", "x", "y", 0}).validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := (Name{"a", strings.Repeat("x", 250), "y", 1}).validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

// SM-OBS-004: human-action lifecycle events carry safe metadata only. The payload's fields are an allowlist with no
// title, body, request, response, comment or reason.
func TestHumanActionPayloadIsSafeMetadata(t *testing.T) {
	allowed := map[string]bool{"space_id": true, "action_id": true, "type": true, "type_slug": true, "type_version": true, "status": true,
		"outcome": true, "priority": true, "requester_kind": true, "task_id": true, "blocks_task": true, "follow_up_of": true}
	h := HumanAction{SpaceID: "spc_a", ActionID: "hum_1", Type: "custom", TypeSlug: "deploy", TypeVersion: 2, Status: "resolved", Outcome: "approved",
		Priority: "high", RequesterKind: "agent", TaskID: "tsk_1", BlocksTask: true, FollowUpOf: "hum_0"}
	for _, status := range []string{"open", "in_progress", "resolved", "cancelled", "expired"} {
		h.Status = status
		p := HumanEvent(h)
		b, _ := json.Marshal(p)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if !allowed[k] {
				t.Errorf("%s: field %q is not safe metadata", status, k)
			}
		}
		if p.eventName().Area != "human" || p.spaceID() != "spc_a" {
			t.Errorf("%s: %v", status, p.eventName())
		}
	}
	if HumanEvent(HumanAction{Status: "weird"}) != nil {
		t.Fatal("unknown status published")
	}
}

func TestDisabledRecordsNothing(t *testing.T) {
	e := newEnv(t, false)
	if ids := e.enqueue(t, 2); ids[0] != "" {
		t.Fatalf("ids %v", ids)
	}
	if st := e.stats(t); st.Pending != 0 {
		t.Fatalf("pending %d", st.Pending)
	}
	var nilBox *Outbox
	if nilBox.Enabled() || nilBox.Source() != "" {
		t.Fatal("nil outbox enabled")
	}
	nilBox.Kick()
	nilBox.RefreshMetrics(e.ctx)
	if n, err := nilBox.Purge(e.ctx); n != 0 || err != nil {
		t.Fatal(n, err)
	}
	if st, err := nilBox.Status(e.ctx); err != nil || st.Enabled {
		t.Fatal(st, err)
	}
}

// An event commits or rolls back with the change that records it, and the relay is kicked only after the commit.
func TestEnqueueIsTransactional(t *testing.T) {
	e := newEnv(t, true)
	boom := errors.New("rolled back")
	var rolled string
	err := e.s.W().Tx(e.ctx, func(tx *store.Tx) error {
		id, err := e.o.Enqueue(e.ctx, tx, Event{Payload: task("tsk_x")})
		rolled = id
		if err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := e.s.R().GetEventOutbox(e.ctx, rolled); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rolled-back event stored: %v", err)
	}
	select {
	case <-e.o.kick:
		t.Fatal("kicked for a rolled-back transaction")
	default:
	}
	ctx := obs.WithIDs(e.ctx, obs.HTTPRequestID("req_abc"))
	var id string
	err = e.s.W().Tx(ctx, func(tx *store.Tx) error {
		var err error
		id, err = e.o.Enqueue(ctx, tx, Event{Payload: task("tsk_y"), Correlation: "bad value\r\nx: y"})
		if len(e.o.kick) != 0 {
			t.Error("kicked inside the transaction")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.o.kick:
	default:
		t.Fatal("no kick after commit")
	}
	r := e.row(t, id)
	if !ids.Valid(ids.Event, id) || r.Subject != "evt.spaceman.queue.task.status_changed" || r.Type != "spaceman.queue.task_status_changed.v1" ||
		r.SpaceID != "spc_a" || r.CorrelationID != "" || r.CausationID != "req_abc" || r.Attempts != 0 {
		t.Fatalf("row %+v", r)
	}
	if err := e.s.W().Tx(e.ctx, func(tx *store.Tx) error {
		_, err := e.o.Enqueue(e.ctx, tx, Event{})
		return err
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty event: %v", err)
	}
}

// The relay publishes committed events with their stored subject, type and payload, and marks them published once.
func TestRelayPublishesAfterCommit(t *testing.T) {
	e := newEnv(t, true)
	ids := e.enqueue(t, 3)
	pub := &fakePub{}
	n, failed, err := e.o.Once(e.ctx, pub)
	if err != nil || n != 3 || failed != 0 {
		t.Fatalf("published %d failed %d err %v", n, failed, err)
	}
	got := pub.got[0]
	var p TaskStatusChanged
	if err := json.Unmarshal(got.data, &p); err != nil {
		t.Fatal(err)
	}
	if got.subject != TaskStatusChangedName.Subject() || got.typ != TaskStatusChangedName.Type() || p.TaskID != "tsk_a" || p.From != "ready" {
		t.Fatalf("published %+v %+v", got, p)
	}
	if r := e.row(t, ids[0]); !r.PublishedAt.Valid || r.Attempts != 1 || r.LastError.Valid {
		t.Fatalf("row %+v", r)
	}
	if n, _, _ := e.o.Once(e.ctx, pub); n != 0 || pub.count() != 3 {
		t.Fatalf("republished %d", n)
	}
	st, err := e.o.Status(e.ctx)
	if err != nil || st.Pending != 0 || st.Published != 3 || st.PublishedSinceStart != 3 || st.LastPublishedAt == nil || st.LagSeconds != 0 || !st.Enabled ||
		st.Source != "spaceman" || st.RetentionHours != 168 {
		t.Fatalf("status %+v %v", st, err)
	}
}

// Pulse being down never loses events: a failure ends the round, the row backs off exponentially (same ID on every
// attempt), and the events publish once Pulse is back.
func TestRelayRetriesWithBackoff(t *testing.T) {
	e := newEnv(t, true)
	ids := e.enqueue(t, 3)
	pub := &fakePub{fails: 2}
	if n, failed, err := e.o.Once(e.ctx, pub); err != nil || n != 0 || failed != 1 {
		t.Fatalf("first round %d %d %v", n, failed, err)
	}
	r := e.row(t, ids[0])
	delay := time.UnixMicro(r.NextAttemptAt).Sub(e.clk.Now())
	if r.Attempts != 1 || !r.LastError.Valid || delay < 500*time.Millisecond || delay > 1500*time.Millisecond {
		t.Fatalf("after one failure: attempts %d delay %s err %v", r.Attempts, delay, r.LastError)
	}
	if e.row(t, ids[1]).Attempts != 0 {
		t.Fatal("the round did not stop after the first failure")
	}
	// The failed row is not due yet; the next one is tried (and fails), the third waits.
	if _, failed, _ := e.o.Once(e.ctx, pub); failed != 1 || e.row(t, ids[1]).Attempts != 1 {
		t.Fatalf("second round failed %d", failed)
	}
	st, _ := e.o.Status(e.ctx)
	if st.Retrying != 2 || st.Pending != 3 || st.FailedSinceStart != 2 || st.LastError == "" || st.LastErrorAt == nil || len(st.RecentFailures) != 2 {
		t.Fatalf("status %+v", st)
	}
	e.clk.Advance(2 * time.Second)
	if n, failed, err := e.o.Once(e.ctx, pub); err != nil || n != 3 || failed != 0 {
		t.Fatalf("recovery %d %d %v", n, failed, err)
	}
	if r := e.row(t, ids[0]); r.Attempts != 2 || !r.PublishedAt.Valid || r.LastError.Valid {
		t.Fatalf("row %+v", r)
	}
}

func TestBackoff(t *testing.T) {
	for _, c := range []struct {
		n      int64
		j      float64
		lo, hi time.Duration
	}{{0, 0, 500 * time.Millisecond, 500 * time.Millisecond}, {1, 0.5, time.Second, time.Second}, {3, 0, 2 * time.Second, 2 * time.Second},
		{4, 0.999, 8 * time.Second, 12 * time.Second}, {60, 0.9, 5 * time.Minute, 5 * time.Minute}} {
		if d := Backoff(time.Second, 5*time.Minute, c.n, c.j); d < c.lo || d > c.hi {
			t.Errorf("Backoff(%d, %v) = %s", c.n, c.j, d)
		}
	}
	for range 100 {
		if j := jitter(); j < 0 || j >= 1 {
			t.Fatal(j)
		}
	}
}

// After MaxAge (or MaxAttempts) a failing event is dead-lettered and no longer retried.
func TestRelayDeadLetters(t *testing.T) {
	e := newEnv(t, true)
	ids := e.enqueue(t, 1)
	pub := &fakePub{fails: -1}
	if _, _, err := e.o.Once(e.ctx, pub); err != nil {
		t.Fatal(err)
	}
	e.clk.Advance(DefaultMaxAge + time.Minute)
	if _, failed, err := e.o.Once(e.ctx, pub); err != nil || failed != 1 {
		t.Fatal(failed, err)
	}
	if r := e.row(t, ids[0]); !r.DeadAt.Valid || r.Attempts != 2 {
		t.Fatalf("row %+v", r)
	}
	if _, failed, _ := e.o.Once(e.ctx, pub); failed != 0 {
		t.Fatal("dead event retried")
	}
	st, _ := e.o.Status(e.ctx)
	if st.Dead != 1 || st.Pending != 0 || len(st.RecentFailures) != 1 || st.RecentFailures[0].DeadAt == nil {
		t.Fatalf("status %+v", st)
	}
	// MaxAttempts dead-letters too, and a panicking publisher is a failure, not a crash.
	e2 := newEnv(t, true)
	e2.o.o.MaxAttempts = 1
	e2.enqueue(t, 1)
	if _, failed, err := e2.o.Once(e2.ctx, &fakePub{panic: true}); err != nil || failed != 1 || e2.stats(t).Dead != 1 {
		t.Fatal(failed, err)
	}
}

// Published and dead-lettered rows go after the retention; pending rows stay while publication is enabled.
func TestPurge(t *testing.T) {
	e := newEnv(t, true)
	ids := e.enqueue(t, 3)
	if n, _, _ := e.o.Once(e.ctx, &fakePub{}); n != 3 {
		t.Fatal(n)
	}
	e.enqueue(t, 1) // pending
	e.clk.Advance(DefaultRetention + time.Hour)
	n, err := e.o.Purge(e.ctx)
	if err != nil || n != 3 {
		t.Fatalf("purged %d %v", n, err)
	}
	if _, err := e.s.R().GetEventOutbox(e.ctx, ids[0]); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("published row kept")
	}
	if st := e.stats(t); st.Pending != 1 {
		t.Fatalf("pending %d", st.Pending)
	}
	// Once publication is turned off, rows left pending are dropped after the retention.
	off := New(e.s, Options{Enabled: false, Clock: e.clk})
	if n, err := off.Purge(e.ctx); err != nil || n != 1 {
		t.Fatalf("stale purge %d %v", n, err)
	}
}

// SM-OBS-004 lag and failure visibility: pending, dead-letter and lag gauges come from the database.
func TestMetrics(t *testing.T) {
	e := newEnv(t, true)
	e.enqueue(t, 2)
	e.clk.Advance(90 * time.Second)
	e.o.RefreshMetrics(e.ctx)
	if v := metricstest.Value(t, e.m, "spaceman_events_pending"); v != 2 {
		t.Fatalf("pending %v", v)
	}
	if v := metricstest.Value(t, e.m, "spaceman_events_lag_seconds"); v != 90 {
		t.Fatalf("lag %v", v)
	}
	if _, _, err := e.o.Once(e.ctx, &fakePub{fails: 1}); err != nil {
		t.Fatal(err)
	}
	if v := metricstest.Value(t, e.m, "spaceman_events_publish_total", "outcome", "retry"); v != 1 {
		t.Fatalf("retry %v", v)
	}
	e.clk.Advance(time.Minute)
	if _, _, err := e.o.Once(e.ctx, &fakePub{}); err != nil {
		t.Fatal(err)
	}
	e.o.RefreshMetrics(e.ctx)
	if v := metricstest.Value(t, e.m, "spaceman_events_publish_total", "outcome", "published"); v != 2 {
		t.Fatalf("published %v", v)
	}
	if p, l := metricstest.Value(t, e.m, "spaceman_events_pending"), metricstest.Value(t, e.m, "spaceman_events_lag_seconds"); p != 0 || l != 0 {
		t.Fatalf("pending %v lag %v", p, l)
	}
	st, _ := e.o.Status(e.ctx)
	if st.FailedSinceStart != 1 || st.PublishedSinceStart != 2 {
		t.Fatalf("status %+v", st)
	}
}

type countingDialer struct {
	mu    sync.Mutex
	fails int
	dials int
	pub   *fakePub
	close int
}

func (d *countingDialer) dial(context.Context) (Publisher, func(), error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.dials++
	if d.fails > 0 {
		d.fails--
		return nil, nil, errors.New("connection refused")
	}
	return d.pub, func() { d.mu.Lock(); d.close++; d.mu.Unlock() }, nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	// Generous: the conditions are reached in milliseconds, but a loaded machine can stall the relay goroutine.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Run reconnects while Pulse is unreachable, publishes on the kick after a commit, and retries on its timer.
func TestRunKickAndRetryTimer(t *testing.T) {
	s := storetest.Open(t)
	o := New(s, Options{Enabled: true, BaseBackoff: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, ReconnectInterval: 20 * time.Millisecond})
	d := &countingDialer{fails: 1, pub: &fakePub{fails: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { o.Run(ctx, d.dial); close(done) }()
	eventually(t, "connected", func() bool { st, _ := o.Status(ctx); return st.Connected })
	if err := s.W().Tx(ctx, func(tx *store.Tx) error {
		_, err := o.Enqueue(ctx, tx, Event{Payload: task("tsk_run")})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The first publish fails; the retry timer (not another kick) publishes it. The publisher sees the event before the
	// relay marks the row published in its own transaction, so wait for the row too: under heavy load that window was
	// long enough for a status read in between to still count the event as pending.
	eventually(t, "published after retry and marked", func() bool {
		st, err := o.Status(ctx)
		return err == nil && d.pub.count() == 1 && st.Pending == 0
	})
	st, _ := o.Status(ctx)
	if st.FailedSinceStart != 1 || st.Pending != 0 || d.pub.count() != 1 {
		t.Fatalf("status %+v, published %d", st, d.pub.count())
	}
	cancel()
	<-done
	if d.dials != 2 || d.close != 1 {
		t.Fatalf("dials %d closes %d", d.dials, d.close)
	}
	if st, _ := o.Status(context.Background()); st.Connected {
		t.Fatal("still connected after stop")
	}
}

func TestCleanID(t *testing.T) {
	for in, want := range map[string]string{"tsk_1": "tsk_1", "github:org/repo#42": "github:org/repo#42", "a b": "", "x\r\ny": "", "": "",
		strings.Repeat("a", 129): ""} {
		if got := CleanID(in); got != want {
			t.Errorf("CleanID(%q) = %q", in, got)
		}
	}
}
