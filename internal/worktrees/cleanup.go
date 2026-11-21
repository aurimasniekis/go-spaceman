package worktrees

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/repos"
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
	Finished int // owned worktrees whose run ended and whose tasks are all done or cancelled, less those whose last removal was refused at their current HEAD
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
	if l.Stale {
		return sum, nil // the report is from the space's earlier node: nothing is said about worktrees that may be gone
	}
	now := svc.clk.Now()
	// A worktree whose last removal the node refused at its current HEAD stays refused until it changes: it is not work
	// for control to be asked about (the same lookup the automatic pass orders by).
	refused, err := svc.refusedHeads(ctx, spaceID, now.AddDate(0, 0, -MinRefusalWindowDays))
	if err != nil {
		return sum, err
	}
	for _, e := range l.Worktrees {
		sum.Total++
		if !e.Owned {
			sum.Unowned++
		}
		if !e.Prunable {
			continue
		}
		if h, ok := refused[[2]string{e.Repo, e.Name}]; ok && h != "" && h == e.Head {
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

// NotifyCleanup sends the space's control the message asking it to tidy the space's repositories, once per period of
// every days (ADR-0107 decision 5): nothing when every is 0 or less, when this period's message was already sent
// (checked first, with one read, so a daily pass costs a space no listing), or when the space has no finished worktree that was not refused at its current HEAD.
// A space that is not active, one whose node reported no worktree and one whose report is from an earlier node get none.
// The message only asks: nothing is removed by it. It is work waiting for control, so with control_autostart on it
// starts the space's control when none runs. It reports whether a message was sent.
func (svc *Service) NotifyCleanup(ctx context.Context, spaceID string, every int64) (bool, error) {
	if every <= 0 || svc.Control == nil {
		return false, nil
	}
	sp, err := svc.s.R().GetSpace(ctx, spaceID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && sp.State != "active") {
		return false, nil // a paused or archived space is not asked to tidy
	} else if err != nil {
		return false, err
	}
	key := cleanupKey(svc.clk.Now(), every)
	if sent, err := svc.Control.ControlNotified(ctx, spaceID, key); err != nil || sent {
		return false, err
	}
	// Before the listing joins runs and tasks: a space whose node reported no worktree has nothing to tidy.
	if ov, err := svc.repos.List(ctx, spaceID); err != nil || !hasWorktrees(ov) {
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

func hasWorktrees(ov *repos.Overview) bool {
	for _, rp := range ov.Items {
		if rp.Status != nil && len(rp.Status.Worktrees) > 0 {
			return true
		}
	}
	return false
}

func cleanupBody(sum CleanupSummary, every int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "SpaceMan sends this reminder once per %d-day period. Nothing has been removed.\n\n", every)
	fmt.Fprintf(&b, "- %d worktrees in this space's repositories, %d of them finished (the owning run ended and every task of it is done or cancelled), the oldest for %d days.\n",
		sum.Total, sum.Finished, sum.OldestDays)
	fmt.Fprintf(&b, "- %d were not created by a SpaceMan run; they are never pruned and are removed only when you ask for that one.\n\n", sum.Unowned)
	b.WriteString("Review them with `spaceman_worktrees_list` and tidy the finished ones with `spaceman_worktrees_prune` (it removes nothing without `apply`) ")
	b.WriteString("or `spaceman_worktree_remove` for one worktree. The node refuses any worktree that has unpushed commits, uncommitted or untracked files, or a live run, and says why. ")
	b.WriteString("The counts come from the node's last repository report.\n")
	return b.String()
}
