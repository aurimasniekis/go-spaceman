package outbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/aurimasniekis/go-pulse/pkg/pulse"

	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// Publisher is the part of the Pulse client the relay uses (*pulse.Client).
type Publisher interface {
	Publish(ctx context.Context, subject, typ string, data any, opts ...pulse.PublishOption) (*pulse.PubAck, error)
}

// Backoff is the delay before attempt n+1 (n ≥ 1): base·2^(n-1) scaled by jitter (0.5 + j, j in [0, 1)), capped at
// max. Ported from Hallpass internal/events.
func Backoff(base, maxDelay time.Duration, attempts int64, jitter float64) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := base << min(attempts-1, 20)
	d = time.Duration(float64(d) * (0.5 + jitter))
	return min(d, maxDelay)
}

// Once publishes up to one batch of due events and returns how many were published and how many failed. One failure
// ends the round: Pulse is probably unreachable, and the remaining rows would only time out one by one.
func (o *Outbox) Once(ctx context.Context, pub Publisher) (published, failed int, err error) {
	now := o.o.Clock.Now()
	rows, err := o.s.R().DueEventOutbox(ctx, db.DueEventOutboxParams{Now: now.UnixMicro(), Lim: batch})
	if err != nil {
		return 0, 0, err
	}
	for _, row := range rows {
		created := time.UnixMicro(row.CreatedAt)
		opts := []pulse.PublishOption{pulse.WithID(row.ID), pulse.WithTime(created), pulse.WithHeader(HeaderOrigin, o.o.Source)}
		if row.CorrelationID != "" {
			opts = append(opts, pulse.WithHeader(HeaderCorrelation, row.CorrelationID))
		}
		if row.CausationID != "" {
			opts = append(opts, pulse.WithHeader(HeaderCausation, row.CausationID))
		}
		if row.Depth > 0 {
			opts = append(opts, pulse.WithHeader(HeaderDepth, strconv.FormatInt(row.Depth, 10)))
		}
		pctx, cancel := context.WithTimeout(ctx, o.o.Timeout)
		_, perr := safePublish(pctx, pub, row.Subject, row.Type, json.RawMessage(row.PayloadJson), opts)
		cancel()
		if ctx.Err() != nil {
			return published, failed, ctx.Err()
		}
		if perr == nil {
			published++
			o.o.Metrics.EventPublish(metrics.EventPublished)
			if err := o.mark(ctx, func(tx *store.Tx) error {
				_, err := tx.Q.MarkEventPublished(ctx, db.MarkEventPublishedParams{PublishedAt: micros(tx.NowMicros()), ID: row.ID})
				return err
			}); err != nil {
				return published, failed, err
			}
			o.note(true, nil)
			continue
		}
		failed++
		msg := truncate(perr.Error(), 500)
		attempts := row.Attempts + 1
		dead := attempts >= int64(o.o.MaxAttempts) || now.Sub(created) >= o.o.MaxAge
		if dead {
			o.o.Metrics.EventPublish(metrics.EventDead)
			o.log.Error("event dead-lettered", slog.String("event.id", row.ID), slog.String("event.type", row.Type),
				slog.Int64("attempts", attempts), slog.String("error.message", msg))
		} else {
			o.o.Metrics.EventPublish(metrics.EventRetry)
			o.log.Warn("event publish failed; will retry", slog.String("event.id", row.ID), slog.String("event.type", row.Type),
				slog.Int64("attempts", attempts), slog.String("error.message", msg))
		}
		next := now.Add(Backoff(o.o.BaseBackoff, o.o.MaxBackoff, attempts, jitter()))
		if err := o.mark(ctx, func(tx *store.Tx) error {
			reason := sql.NullString{String: msg, Valid: true}
			if dead {
				_, err := tx.Q.MarkEventDead(ctx, db.MarkEventDeadParams{DeadAt: micros(tx.NowMicros()), LastError: reason, ID: row.ID})
				return err
			}
			_, err := tx.Q.MarkEventRetry(ctx, db.MarkEventRetryParams{NextAttemptAt: next.UnixMicro(), LastError: reason, ID: row.ID})
			return err
		}); err != nil {
			return published, failed, err
		}
		o.note(false, perr)
		break
	}
	return published, failed, nil
}

func safePublish(ctx context.Context, pub Publisher, subject, typ string, data json.RawMessage, opts []pulse.PublishOption) (ack *pulse.PubAck, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errors.New("publisher panicked")
		}
	}()
	return pub.Publish(ctx, subject, typ, data, opts...)
}

func (o *Outbox) mark(ctx context.Context, fn func(tx *store.Tx) error) error {
	return o.s.W().TxNamed(context.WithoutCancel(ctx), "outbox.mark", fn)
}

func (o *Outbox) note(ok bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if ok {
		o.published++
		return
	}
	o.failed++
	o.lastErr, o.lastErrAt = truncate(err.Error(), 500), o.o.Clock.Now()
}

func (o *Outbox) setConnected(c bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.connected = c
	if err != nil {
		o.lastErr, o.lastErrAt = truncate(err.Error(), 500), o.o.Clock.Now()
	}
}

// Dialer connects to Pulse; the returned close function releases the connection.
type Dialer func(ctx context.Context) (Publisher, func(), error)

// Run relays events until ctx ends. It dials Pulse (retrying every ReconnectInterval while that fails; events wait in
// the outbox), then publishes whenever a commit kicks it or the earliest retry falls due. It never polls: between
// rounds it waits on the kick channel and a single timer.
func (o *Outbox) Run(ctx context.Context, dial Dialer) {
	var pub Publisher
	var closeFn func()
	defer func() {
		if closeFn != nil {
			closeFn()
		}
		o.setConnected(false, nil)
	}()
	timer := time.NewTimer(0)
	defer timer.Stop()
	refresh := time.NewTicker(BacklogRefresh)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			o.RefreshMetrics(ctx)
			continue
		case <-o.kick:
		case <-timer.C:
		}
		if pub == nil {
			p, c, err := dial(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				o.setConnected(false, err)
				o.log.Warn("cannot connect to Pulse; events wait in the outbox", slog.String("url.full", o.o.URL), obs.Err(err))
				timer.Reset(o.o.ReconnectInterval)
				continue
			}
			pub, closeFn = p, c
			o.setConnected(true, nil)
			o.log.Info("event relay connected to Pulse", slog.String("url.full", o.o.URL))
		}
		for {
			n, failed, err := o.Once(ctx, pub)
			if err != nil && !errors.Is(err, context.Canceled) {
				o.log.Warn("event relay", obs.Err(err))
			}
			if err != nil || failed > 0 || n < batch {
				break
			}
		}
		o.RefreshMetrics(ctx)
		timer.Reset(o.untilNext(ctx))
	}
}

// untilNext is the wait until the earliest pending retry, or a long idle wait when nothing is pending (a kick
// interrupts it).
func (o *Outbox) untilNext(ctx context.Context) time.Duration {
	const idle = time.Hour
	next, err := o.s.R().NextEventOutboxAttempt(ctx)
	if err != nil {
		return o.o.ReconnectInterval
	}
	if next == 0 {
		return idle
	}
	d := time.UnixMicro(next).Sub(o.o.Clock.Now())
	return min(max(d, 0), idle)
}

// BacklogRefresh is how often Run refreshes the backlog and lag gauges while nothing happens.
const BacklogRefresh = 30 * time.Second

// RefreshMetrics sets the pending, dead-letter and lag gauges from the database.
func (o *Outbox) RefreshMetrics(ctx context.Context) {
	if o == nil || o.o.Metrics == nil {
		return
	}
	st, err := o.s.R().EventOutboxStats(ctx)
	if err != nil {
		return
	}
	o.o.Metrics.SetEventBacklog(st.Pending, st.Dead)
	o.o.Metrics.SetEventLag(lag(o.o.Clock.Now(), st.OldestPendingAt))
}

func lag(now time.Time, oldest int64) time.Duration {
	if oldest == 0 {
		return 0
	}
	return max(now.Sub(time.UnixMicro(oldest)), 0)
}

// Purge deletes rows published or dead-lettered more than the retention ago. Pending rows are kept, unless
// publication is disabled: then rows left pending from when it was enabled are dropped once older than the
// retention (nothing would ever publish them).
func (o *Outbox) Purge(ctx context.Context) (int64, error) {
	if o == nil {
		return 0, nil
	}
	cutoff := o.o.Clock.Now().Add(-o.o.Retention).UnixMicro()
	var n int64
	err := o.s.W().TxNamed(ctx, "outbox.purge", func(tx *store.Tx) error {
		var err error
		if n, err = tx.Q.PurgePublishedEventOutbox(ctx, sql.NullInt64{Int64: cutoff, Valid: true}); err != nil {
			return err
		}
		if !o.o.Enabled {
			m, err := tx.Q.PurgeStaleEventOutbox(ctx, cutoff)
			n += m
			return err
		}
		return nil
	})
	return n, err
}

// Failure is a recent failed event (its last error).
type Failure struct {
	ID        string     `json:"id"`
	Type      string     `json:"type"`
	Attempts  int64      `json:"attempts"`
	LastError string     `json:"last_error"`
	CreatedAt time.Time  `json:"created_at"`
	DeadAt    *time.Time `json:"dead_at,omitempty"`
}

// Status is the publication state shown to administrators (SM-OBS-004: lag and failure visibility).
type Status struct {
	Enabled bool   `json:"enabled" doc:"Whether outbound publication is on (events.outbound.enabled); while off nothing is recorded"`
	Source  string `json:"source" doc:"ce-source and spaceman-origin of published events"`
	URL     string `json:"url,omitempty" doc:"The Pulse URL (never credentials)"`
	// Connected is whether the relay holds a Pulse connection (false while disabled or unreachable).
	Connected bool  `json:"connected" doc:"The relay holds a Pulse connection"`
	Pending   int64 `json:"pending" doc:"Events waiting to be published"`
	Retrying  int64 `json:"retrying" doc:"Pending events that failed at least once"`
	Dead      int64 `json:"dead" doc:"Events dead-lettered after the retry limit"`
	Published int64 `json:"published" doc:"Published events still retained"`
	// LagSeconds is the age of the oldest pending event (0 when none).
	LagSeconds          float64    `json:"lag_seconds" doc:"Age of the oldest pending event; 0 when none"`
	OldestPendingAt     *time.Time `json:"oldest_pending_at,omitempty"`
	LastPublishedAt     *time.Time `json:"last_published_at,omitempty"`
	PublishedSinceStart int64      `json:"published_since_start" doc:"Publish acknowledgements since central started"`
	FailedSinceStart    int64      `json:"failed_since_start" doc:"Failed publish attempts since central started"`
	LastError           string     `json:"last_error,omitempty" doc:"The last connection or publish error"`
	LastErrorAt         *time.Time `json:"last_error_at,omitempty"`
	RecentFailures      []Failure  `json:"recent_failures" doc:"Up to 10 unpublished events with an error, newest first"`
	RetentionHours      int64      `json:"retention_hours" doc:"How long published and dead-lettered events are kept"`
}

// Status returns the publication state.
func (o *Outbox) Status(ctx context.Context) (Status, error) {
	if o == nil {
		return Status{RecentFailures: []Failure{}}, nil
	}
	st, err := o.s.R().EventOutboxStats(ctx)
	if err != nil {
		return Status{}, err
	}
	fails, err := o.s.R().ListRecentEventFailures(ctx, 10)
	if err != nil {
		return Status{}, err
	}
	now := o.o.Clock.Now()
	out := Status{Enabled: o.o.Enabled, Source: o.o.Source, URL: o.o.URL, Pending: st.Pending, Retrying: st.Retrying, Dead: st.Dead,
		Published: st.Published, LagSeconds: lag(now, st.OldestPendingAt).Seconds(), OldestPendingAt: tp(st.OldestPendingAt),
		LastPublishedAt: tp(st.LastPublishedAt), RecentFailures: []Failure{}, RetentionHours: int64(o.o.Retention / time.Hour)}
	for _, f := range fails {
		var dead *time.Time
		if f.DeadAt.Valid {
			dead = tp(f.DeadAt.Int64)
		}
		out.RecentFailures = append(out.RecentFailures, Failure{ID: f.ID, Type: f.Type, Attempts: f.Attempts, LastError: f.LastError.String,
			CreatedAt: time.UnixMicro(f.CreatedAt).UTC(), DeadAt: dead})
	}
	o.mu.Lock()
	out.Connected, out.PublishedSinceStart, out.FailedSinceStart, out.LastError = o.connected, o.published, o.failed, o.lastErr
	if !o.lastErrAt.IsZero() {
		t := o.lastErrAt.UTC()
		out.LastErrorAt = &t
	}
	o.mu.Unlock()
	return out, nil
}

func tp(us int64) *time.Time {
	if us == 0 {
		return nil
	}
	t := time.UnixMicro(us).UTC()
	return &t
}

func micros(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// jitter returns a uniform value in [0, 1).
func jitter() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
