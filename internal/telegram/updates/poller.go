package updates

import (
	"context"
	"log/slog"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Poller defaults.
const (
	DefaultPollTimeout = 30 * time.Second // getUpdates long-poll wait
	pollLimit          = 100              // updates per getUpdates call (the documented maximum)
	pollGrace          = 15 * time.Second // the HTTP call may take this much longer than the long-poll wait
	pollBackoffMin     = time.Second
	pollBackoffMax     = 30 * time.Second
)

// Getter is the part of the Bot API the poller calls. *botapi.Client satisfies it.
type Getter interface {
	GetUpdates(ctx context.Context, p botapi.GetUpdatesParams) ([]botapi.Update, error)
}

// PollerOptions configure a Poller.
type PollerOptions struct {
	// Timeout is the long-poll wait (0: DefaultPollTimeout).
	Timeout time.Duration
	Metrics *metrics.Metrics
	Log     *slog.Logger
	// Sleep waits between failed calls (tests; nil: a timer that ends early with ctx).
	Sleep func(ctx context.Context, d time.Duration) error
}

// Poller is the long-polling mode: it calls getUpdates and feeds the same Intake the webhook does. It confirms a batch
// (the next call's offset) only after every update of it is queued, waiting while the queue is full, so polling never
// drops an update for backpressure. The webhook must be deleted first: Telegram refuses getUpdates while one is set.
type Poller struct {
	bot     Getter
	intake  *Intake
	timeout time.Duration
	metrics *metrics.Metrics
	log     *slog.Logger
	sleep   func(ctx context.Context, d time.Duration) error
}

// NewPoller returns a poller feeding intake.
func NewPoller(bot Getter, intake *Intake, o PollerOptions) *Poller {
	if o.Timeout <= 0 {
		o.Timeout = DefaultPollTimeout
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	return &Poller{bot: bot, intake: intake, timeout: o.Timeout, metrics: o.Metrics, log: o.Log, sleep: o.Sleep}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run polls until ctx ends and returns ctx's error. Failed calls are retried after Telegram's retry_after when it
// gives one, otherwise after a backoff from 1 s doubling to 30 s.
func (p *Poller) Run(ctx context.Context) error {
	var offset int64
	backoff := pollBackoffMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		callCtx, cancel := context.WithTimeout(ctx, p.timeout+pollGrace)
		batch, err := p.bot.GetUpdates(callCtx, botapi.GetUpdatesParams{Offset: offset, Limit: pollLimit,
			Timeout: int(p.timeout / time.Second), AllowedUpdates: AllowedUpdates})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.metrics.TelegramPollError()
			wait := backoff
			if ra, ok := botapi.RetryAfter(err); ok {
				wait = ra
			} else {
				backoff = min(backoff*2, pollBackoffMax)
			}
			p.log.Warn("telegram: getUpdates failed", slog.String("error", err.Error()), slog.Duration("retry_in", wait))
			if err := p.sleep(ctx, wait); err != nil {
				return err
			}
			continue
		}
		backoff = pollBackoffMin
		for _, u := range batch {
			if u.UpdateID < offset {
				continue // already confirmed: Telegram shouldn't send it again
			}
			if _, err := p.intake.Put(ctx, u); err != nil {
				return err
			}
			offset = u.UpdateID + 1
		}
	}
}
