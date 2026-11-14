// Package updates receives Telegram updates and dispatches them (ADR-0083 decision 9, SM-TG-004).
//
// Two modes feed one Intake: the webhook (Webhook, an internet-facing route outside the API handler) and long polling
// (Poller, outbound only). Telegram allows only one of them at a time. The Intake drops update_ids it has already
// accepted, and holds accepted updates in a bounded queue sharded by chat, so one chat's updates are handled in order
// by one worker. When a shard is full the webhook answers 503 and Telegram retries later (backpressure, not loss); the
// poller waits instead of asking for more.
//
// Restart loss is accepted: an update the webhook already answered with 200 but that is still queued is lost if
// central stops, and Telegram won't resend it. So every capture a later handler makes (an inbox item, a response, a
// comment) must end with an explicit acknowledgement message or card edit, and a user who sees none knows to resend.
//
// Nothing in this package reads the database. The Dispatcher looks links up in memory (Links), and an update from a
// Telegram user without a link gets at most the linking instructions; nothing is read on its behalf.
package updates

import (
	"context"
	"log/slog"
	"sync"

	"golang.org/x/time/rate"

	"github.com/aurimasniekis/go-spaceman/internal/obs/metrics"
	"github.com/aurimasniekis/go-spaceman/internal/telegram/botapi"
)

// Defaults (ADR-0083 decision 9).
const (
	DefaultQueueSize  = 1024 // accepted updates waiting, over every shard
	DefaultWorkers    = 4    // shards, one worker each
	DefaultDedupeSize = 4096 // the most recent update_ids remembered
	DefaultRate       = 20   // updates handled per second, over every worker
)

// AllowedUpdates are the update kinds SpaceMan asks Telegram for (setWebhook and getUpdates allowed_updates).
var AllowedUpdates = []string{"message", "callback_query", "my_chat_member"}

// Result is what the Intake did with an offered update.
type Result int

// Offer results.
const (
	Accepted  Result = iota // queued for a worker
	Duplicate               // its update_id was accepted before: dropped
	Full                    // its shard is full: not queued, not remembered, so a retry can succeed
)

// IntakeOptions configure an Intake. Zero values take the defaults.
type IntakeOptions struct {
	QueueSize  int
	Workers    int
	DedupeSize int
	// Rate bounds how many updates per second the workers handle together (0: DefaultRate; negative: unlimited).
	Rate    float64
	Metrics *metrics.Metrics
	Log     *slog.Logger
}

// Intake dedupes update_ids and queues updates for its workers.
type Intake struct {
	shards  []chan botapi.Update
	limiter *rate.Limiter
	metrics *metrics.Metrics
	log     *slog.Logger

	mu   sync.Mutex
	seen map[int64]struct{}
	ring []int64 // the remembered IDs in arrival order; ring[next] is the oldest once full
	next int
}

// NewIntake returns an Intake; call Run to start its workers.
func NewIntake(o IntakeOptions) *Intake {
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.Workers <= 0 {
		o.Workers = DefaultWorkers
	}
	o.Workers = min(o.Workers, o.QueueSize)
	if o.DedupeSize <= 0 {
		o.DedupeSize = DefaultDedupeSize
	}
	if o.Rate == 0 {
		o.Rate = DefaultRate
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	in := &Intake{metrics: o.Metrics, log: o.Log, seen: make(map[int64]struct{}, o.DedupeSize), ring: make([]int64, 0, o.DedupeSize)}
	if o.Rate > 0 {
		in.limiter = rate.NewLimiter(rate.Limit(o.Rate), max(1, int(o.Rate)))
	}
	per := o.QueueSize / o.Workers
	for range o.Workers {
		in.shards = append(in.shards, make(chan botapi.Update, per))
	}
	return in
}

// SetRate changes how many updates per second the workers handle together, at once (the burst follows the rate). It
// does nothing for an Intake built without a rate limit.
func (in *Intake) SetRate(r float64) {
	if in.limiter == nil || r <= 0 {
		return
	}
	in.limiter.SetLimit(rate.Limit(r))
	in.limiter.SetBurst(max(1, int(r)))
}

// chatOf is the chat an update belongs to, which picks its shard (0 when it names none).
func chatOf(u *botapi.Update) int64 {
	switch {
	case u.Message != nil:
		return u.Message.Chat.ID
	case u.EditedMessage != nil:
		return u.EditedMessage.Chat.ID
	case u.CallbackQuery != nil:
		if m := u.CallbackQuery.Message; m != nil {
			return m.Chat.ID
		}
		return u.CallbackQuery.From.ID
	case u.MyChatMember != nil:
		return u.MyChatMember.Chat.ID
	}
	return 0
}

func (in *Intake) shard(u *botapi.Update) chan botapi.Update {
	c := uint64(chatOf(u)) //nolint:gosec // G115: only the bits matter for picking a shard
	return in.shards[c%uint64(len(in.shards))]
}

// rememberLocked records id as accepted, forgetting the oldest remembered ID when the ring is full.
func (in *Intake) rememberLocked(id int64) {
	if len(in.ring) < cap(in.ring) {
		in.ring = append(in.ring, id)
	} else {
		delete(in.seen, in.ring[in.next])
		in.ring[in.next] = id
		in.next = (in.next + 1) % len(in.ring)
	}
	in.seen[id] = struct{}{}
}

// Offer queues u without blocking (the webhook).
func (in *Intake) Offer(u botapi.Update) Result {
	in.mu.Lock()
	defer in.mu.Unlock()
	if _, ok := in.seen[u.UpdateID]; ok {
		return Duplicate
	}
	select {
	case in.shard(&u) <- u:
	default:
		return Full
	}
	in.rememberLocked(u.UpdateID)
	in.metrics.TelegramQueued(1)
	return Accepted
}

// Put queues u, waiting while its shard is full (the poller). It returns Duplicate or Accepted, or ctx's error.
func (in *Intake) Put(ctx context.Context, u botapi.Update) (Result, error) {
	in.mu.Lock()
	if _, ok := in.seen[u.UpdateID]; ok {
		in.mu.Unlock()
		return Duplicate, nil
	}
	// Remember it before waiting, so a concurrent offer of the same ID is a duplicate rather than a second copy.
	in.rememberLocked(u.UpdateID)
	in.mu.Unlock()
	select {
	case in.shard(&u) <- u:
		in.metrics.TelegramQueued(1)
		return Accepted, nil
	case <-ctx.Done():
		return Full, ctx.Err()
	}
}

// Run starts one worker per shard, each calling handle for its updates in order, and returns when ctx ends. Updates
// still queued then are dropped (restart loss, see the package comment). A panicking handle is recovered and counted,
// so one bad update can't stop the worker.
func (in *Intake) Run(ctx context.Context, handle func(context.Context, botapi.Update)) {
	var wg sync.WaitGroup
	for _, ch := range in.shards {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case u := <-ch:
					in.metrics.TelegramQueued(-1)
					if in.limiter != nil && in.limiter.Wait(ctx) != nil {
						return
					}
					in.handle(ctx, handle, u)
				}
			}
		})
	}
	wg.Wait()
}

func (in *Intake) handle(ctx context.Context, handle func(context.Context, botapi.Update), u botapi.Update) {
	defer func() {
		if r := recover(); r != nil {
			// Never log the update or the panic value: either may carry message text.
			in.log.Error("telegram update handler panicked", slog.Int64("update_id", u.UpdateID))
			in.metrics.TelegramUpdate(metrics.TelegramUpdateFailed)
		}
	}()
	handle(ctx, u)
}
