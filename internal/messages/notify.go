package messages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// NoticeQueue queues idle-gated terminal input for a run (fleet.Service).
type NoticeQueue interface {
	QueueNotice(ctx context.Context, tx *store.Tx, runID, text string) (string, error)
	KickRun(ctx context.Context, runID string)
}

// Notifier is the terminal-notice delivery adapter (SM-COMM-003): when a run's program is idle and something waits for
// it, it types a one-line notice into the terminal through safe input (refused by the node while the program is
// busy). The notice only says that something is waiting; the content is read through the agent's tools, and
// acknowledgement happens there too. It is event-driven (new messages, new human responses, a run turning idle) with a
// slow sweep for retries; nothing polls per space.
type Notifier struct {
	svc   *Service
	q     NoticeQueue
	log   *slog.Logger
	clock clock.Clock
	wake  chan struct{}

	mu   sync.Mutex
	last map[string]time.Time
}

// Notice timing.
const (
	noticeEvery  = time.Minute     // at most one notice per run per minute
	noticeRetry  = 5 * time.Minute // an item noticed but not fetched is noticed again after this
	sweepEvery   = time.Minute
	noticePrefix = "SpaceMan:"
)

// NewNotifier returns a notifier; Run starts it.
func NewNotifier(svc *Service, q NoticeQueue, log *slog.Logger, c clock.Clock) *Notifier {
	if c == nil {
		c = clock.System{}
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	n := &Notifier{svc: svc, q: q, log: obs.Component(log, "notifier"), clock: c, wake: make(chan struct{}, 1), last: map[string]time.Time{}}
	svc.Kick = n.Kick
	return n
}

// Kick asks for a sweep soon.
func (n *Notifier) Kick() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Run sweeps until ctx ends.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(sweepEvery)
	defer t.Stop()
	for {
		if err := n.Sweep(ctx); err != nil && ctx.Err() == nil {
			n.log.Warn("notice sweep", obs.Err(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-n.wake:
		case <-t.C:
		}
	}
}

// Sweep notifies every idle run that has something waiting.
func (n *Notifier) Sweep(ctx context.Context) error {
	runs, err := n.svc.s.R().ListNoticeCandidates(ctx)
	if err != nil {
		return err
	}
	now := n.clock.Now()
	for _, r := range runs {
		n.mu.Lock()
		recent := now.Sub(n.last[r.ID]) < noticeEvery
		n.mu.Unlock()
		if recent {
			continue
		}
		sent, err := n.notify(ctx, r, now)
		if errors.Is(err, fleet.ErrTakenOver) {
			continue // an operator has taken over the terminal: the notice waits until the takeover ends (SM-UI-004)
		}
		if err != nil {
			n.log.Warn("notice", slog.String(obs.FieldRunID, r.ID), obs.Err(err))
			continue
		}
		if sent {
			n.mu.Lock()
			n.last[r.ID] = now
			n.mu.Unlock()
			n.q.KickRun(ctx, r.ID)
		}
	}
	return nil
}

func due(noticed sql.NullInt64, now time.Time) bool {
	return !noticed.Valid || now.Sub(time.UnixMicro(noticed.Int64)) >= noticeRetry
}

func (n *Notifier) notify(ctx context.Context, r db.ListNoticeCandidatesRow, now time.Time) (bool, error) {
	sent := false
	err := n.svc.s.W().TxNamed(ctx, "messages.notice", func(tx *store.Tx) error {
		msgs, err := tx.Q.ListMessagesFor(ctx, db.ListMessagesForParams{SpaceID: r.SpaceID, RunID: r.ID, Role: r.Role, IsControl: b2i(r.Kind == "control"), Lim: 100})
		if err != nil {
			return err
		}
		var mids []string
		for _, m := range msgs {
			if m.State == Stored && !sentBy(m, r.ID) && due(m.NoticedAt, now) {
				mids = append(mids, m.ID)
			}
		}
		ns, err := tx.Q.ListHumanNotificationsFor(ctx, db.ListHumanNotificationsForParams{SpaceID: r.SpaceID, RunID: r.ID, IsControl: b2i(r.Kind == "control"), Lim: 100})
		if err != nil {
			return err
		}
		var nids []string
		for _, x := range ns {
			// wake 0: the policy says not to type into the terminal for it; the run reads it when it checks its messages.
			if x.Wake == 1 && due(x.NoticedAt, now) {
				nids = append(nids, x.ID)
			}
		}
		if len(mids) == 0 && len(nids) == 0 {
			return nil
		}
		cmd, err := n.q.QueueNotice(ctx, tx, r.ID, noticeText(len(mids), len(nids)))
		if err != nil {
			return err
		}
		at := sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
		if len(mids) > 0 {
			if err := tx.Q.SetMessageNotice(ctx, db.SetMessageNoticeParams{Command: cmd, Now: at, Ids: mids}); err != nil {
				return err
			}
		}
		if len(nids) > 0 {
			if err := tx.Q.SetNotificationNotice(ctx, db.SetNotificationNoticeParams{Command: cmd, Now: at, Ids: nids}); err != nil {
				return err
			}
		}
		sent = true
		return nil
	})
	return sent, err
}

// noticeText is the single line typed into the terminal: it carries no message content (that is read through the
// tools, as data rather than as a prompt).
func noticeText(msgs, human int) string {
	var parts []string
	if msgs > 0 {
		parts = append(parts, plural(msgs, "new message", "new messages"))
	}
	if human > 0 {
		parts = append(parts, plural(human, "human response", "human responses"))
	}
	return fmt.Sprintf("%s %s waiting for you. Call the spaceman_messages tool to read them, then acknowledge each with spaceman_message_ack.",
		noticePrefix, strings.Join(parts, " and "))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// OnCommandAck marks the items a notice announced as available once the node accepted it (fleet.Options.OnCommandAck).
// A refused notice (the program was busy) leaves them stored; the next idle report or sweep tries again.
func (n *Notifier) OnCommandAck(ctx context.Context, tx *store.Tx, commandID, result string) error {
	if result != proto.AckAccepted {
		return nil
	}
	now := sql.NullInt64{Int64: tx.NowMicros(), Valid: true}
	rows, err := tx.Q.ListMessagesByNotice(ctx, commandID)
	if err != nil || len(rows) == 0 {
		if err == nil {
			_, err = tx.Q.MarkNotificationNoticeAccepted(ctx, db.MarkNotificationNoticeAcceptedParams{Now: now, Command: commandID})
		}
		return err
	}
	if _, err := tx.Q.MarkNoticeAccepted(ctx, db.MarkNoticeAcceptedParams{Now: now, Command: commandID}); err != nil {
		return err
	}
	if _, err := tx.Q.MarkNotificationNoticeAccepted(ctx, db.MarkNotificationNoticeAcceptedParams{Now: now, Command: commandID}); err != nil {
		return err
	}
	for _, m := range rows {
		if err := n.svc.changes.Record(ctx, tx, m.SpaceID, "message", m.ID, "updated", m.Version+1); err != nil {
			return err
		}
	}
	return nil
}
