package worktrees

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/store"
)

// ControlNotifier sends control a system notification once per dedup key (messages.Service).
type ControlNotifier interface {
	// ControlNotified reports, from a read, whether the notification with this key was sent.
	ControlNotified(ctx context.Context, spaceID, dedup string) (bool, error)
	// NotifyControlTx sends it inside the caller's transaction; false when the key was already used.
	NotifyControlTx(ctx context.Context, tx *store.Tx, spaceID, dedup, subject, body string) (bool, error)
}

// CleanupMessageSubject is the subject of the cleanup message.
const CleanupMessageSubject = "Tidy this space's repositories"

// CleanupSummary is what the cleanup message says about a space's worktrees.
type CleanupSummary struct {
	Total    int // worktrees directly inside worktrees/
	Finished int // owned worktrees whose run ended and whose tasks are all done or cancelled (spaceman_worktrees_prune would try them)
	Unowned  int // worktrees no SpaceMan run created: never pruned, only removed on request
	// OldestDays is the age in whole days of the oldest finished worktree, counted from the later of its run's end and
	// its tasks' latest finish time.
	OldestDays int
}

// CleanupSummary counts a space's worktrees from the last repository report (one listing: bounded by the listing's
// 1000-run window).
func (svc *Service) CleanupSummary(ctx context.Context, spaceID string) (CleanupSummary, error) {
	var sum CleanupSummary
	l, err := svc.List(ctx, spaceID, "")
	if err != nil {
		return sum, err
	}
	now := svc.clk.Now()
	for _, e := range l.Worktrees {
		sum.Total++
		if !e.Owned {
			sum.Unowned++
		}
		if !e.Prunable {
			continue
		}
		sum.Finished++
		if since := finishedSince(e); since != nil {
			sum.OldestDays = max(sum.OldestDays, int(now.Sub(*since)/(24*time.Hour)))
		}
	}
	return sum, nil
}

// cleanupKey is the dedup key of the message of the period now falls in: every every days days, counted from the Unix
// epoch, so a restart or a second central sends it once per period.
func cleanupKey(now time.Time, every int64) string {
	return fmt.Sprintf("worktree-cleanup:%dd:%d", every, now.Unix()/86400/every)
}

// NotifyCleanup sends the space's control the message asking it to tidy the space's repositories, at most once every
// every days (ADR-0107 decision 5): nothing when every is 0 or less, when this period's message was already sent
// (checked first, with one read, so a daily pass costs a space no listing), or when the space has no finished worktree.
// The message only asks: nothing is removed by it. It reports whether a message was sent.
func (svc *Service) NotifyCleanup(ctx context.Context, spaceID string, every int64) (bool, error) {
	if every <= 0 || svc.Control == nil {
		return false, nil
	}
	key := cleanupKey(svc.clk.Now(), every)
	if sent, err := svc.Control.ControlNotified(ctx, spaceID, key); err != nil || sent {
		return false, err
	}
	sum, err := svc.CleanupSummary(ctx, spaceID)
	if err != nil || sum.Finished == 0 {
		return false, err
	}
	sent := false
	err = svc.s.W().TxNamed(ctx, "worktrees.cleanup_notice", func(tx *store.Tx) error {
		var err error
		sent, err = svc.Control.NotifyControlTx(ctx, tx, spaceID, key, CleanupMessageSubject, cleanupBody(sum, every))
		return err
	})
	return sent && err == nil, err
}

func cleanupBody(sum CleanupSummary, every int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SpaceMan sends this reminder at most once every %d days. Nothing has been removed.\n\n", every)
	fmt.Fprintf(&b, "- %d worktrees in this space's repositories, %d of them finished (the owning run ended and every task of it is done or cancelled), the oldest for %d days.\n",
		sum.Total, sum.Finished, sum.OldestDays)
	fmt.Fprintf(&b, "- %d have no SpaceMan run that created them; they are never pruned and are removed only when you ask for that one.\n\n", sum.Unowned)
	b.WriteString("Review them with `spaceman_worktrees_list` and tidy the finished ones with `spaceman_worktrees_prune` (it removes nothing without `apply`) ")
	b.WriteString("or `spaceman_worktree_remove` for one worktree. The node refuses any worktree that has unpushed commits, uncommitted or untracked files, or a live run, and says why. ")
	b.WriteString("The counts come from the node's last repository report.\n")
	return b.String()
}
