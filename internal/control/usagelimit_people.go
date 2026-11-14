package control

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/human"
	"github.com/aurimasniekis/go-spaceman/internal/obs"
)

// LongHoldDedup is the dedup key of the people notice of a hold: one human action per space and account window.
func LongHoldDedup(limitID string) string { return "usage-limit-human:" + limitID }

// TellLongHolds raises one human action (an informational message, normal priority) in a space for each of its holds
// whose reset is further away than the space's usage_limit_notify_after_hours (ADR-0079 Q5): which account, until
// when, what is waiting on it and what a person can do. It runs on the deadline timer (a limit report wakes it). The
// action is deduplicated on the hold and the hold is marked told after it, so a crash in between raises it once, never
// twice; a 5-hour wait (below the threshold) raises nothing.
func (sv *Supervisor) TellLongHolds(ctx context.Context) (int, error) {
	holds, err := sv.fl.LongHoldsToTell(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, h := range holds {
		title, body := longHoldText(h, sv.clock.Now())
		_, created, err := sv.human.Create(ctx, audit.System, h.Hold.SpaceID, human.CreateInput{Type: human.TypeMessage, Priority: "normal",
			Title: title, Body: body, Request: []byte(`{"requires_ack":false}`),
			Requester:     human.Requester{Kind: "system", Label: "Usage limits"},
			CorrelationID: h.Hold.ID, DedupKey: LongHoldDedup(h.Hold.ID)})
		switch apperr.CodeOf(err) {
		case "":
		case apperr.CodeRateLimited, apperr.CodePrecondition:
			// The space has too many open actions, or was archived since: try again on a later round.
			sv.log.Info("usage-limit people notice deferred", slog.String("usage_limit_id", h.Hold.ID), obs.Err(err))
			continue
		default:
			return n, err
		}
		if err := sv.fl.MarkHoldTold(ctx, h.Hold.ID); err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}

// longHoldText is the notice's title and body.
func longHoldText(h fleet.LongHold, now time.Time) (title, body string) {
	reset := h.Hold.ResetsAt.UTC()
	title = fmt.Sprintf("%s until %s", upperFirst(h.Subject), reset.Format("2006-01-02 15:04 UTC"))
	if utf8.RuneCountInString(title) > 200 {
		title = string([]rune(title)[:199]) + "…"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "In %s, %s. It resets at %s (in about %s).", h.Space, h.Subject, reset.Format(time.RFC3339), approxDuration(reset.Sub(now)))
	if h.Runs == 0 {
		b.WriteString(" No run of the space is waiting on it right now.")
	} else {
		fmt.Fprintf(&b, " %s waiting on it (%s): SpaceMan keeps them, or parks them and resumes them at the reset with their sessions.",
			plural(h.Runs, "run is", "runs are"), withTasks(h.Tasks))
	}
	if h.Hold.Scope == fleet.LimitScopeAccount {
		b.WriteString(" Launches on the account are refused until then.\n\nNothing is needed if the work can wait. To keep it going sooner, bind " +
			"another account of the same harness to the space and turn on usage-limit failover (the space limit usage_limit_failover); if the " +
			"account works again before then, an operator can clear the hold (spaceman limits clear).")
	} else {
		b.WriteString(" Launches through the gateway are refused until then.\n\nNothing is needed if the work can wait. To keep it going sooner, " +
			"add an account or budget in Harbor Pilot; if it works again before then, an operator can clear the hold (spaceman limits clear).")
	}
	fmt.Fprintf(&b, "\n\nYou get one such notice per account and limit window, only for waits longer than %s (the space limit usage_limit_notify_after_hours; 0 turns it off).",
		approxDuration(h.NotifyAfter))
	return title, b.String()
}

func withTasks(n int) string {
	if n == 0 {
		return "none with a task"
	}
	return fmt.Sprintf("%d with a task", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// approxDuration is a wait in whole days and hours ("6 days 4 hours", "12 hours"), at least an hour.
func approxDuration(d time.Duration) string {
	h := max(int(d.Round(time.Hour)/time.Hour), 1)
	days, hours := h/24, h%24
	switch {
	case days == 0:
		return plural(hours, "hour", "hours")
	case hours == 0:
		return plural(days, "day", "days")
	}
	return plural(days, "day", "days") + " " + plural(hours, "hour", "hours")
}

func upperFirst(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[size:]
}
