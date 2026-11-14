package harborpilot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

// Gateway usage-limit waits (ADR-0079 decision 9, UL-8; contract docs/contracts/harbor-pilot.md, "Quota board and the
// all-exhausted 429"). A gateway run's limit report carries no reset: Harbor Pilot answers 429 without Retry-After. The
// reset is estimated from Harbor Pilot itself, outside any transaction:
//   - a quota exhaustion (the connection-wide hold): the earliest `until` among the quota board's unavailable accounts.
//     The board has no project filter, so this is an estimate over the whole installation (Q9);
//   - a blocking budget (a project's hold): the latest `period_end` among the covering blocking budgets.
//
// The estimate is read once when the hold opens and again every 15 minutes (fleet.GatewayRecheck), or at once when
// Harbor Pilot's quota.window.recovered event arrives (Kick). A hold clears when its accounts recover or no budget
// blocks any more, which wakes the runs parked under it.

// AccountHealth is one provider account's routing view on the quota board (Harbor Pilot 0.1.3 or later).
type AccountHealth struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Billing   string `json:"billing"`
	Available bool   `json:"available"`
	// Reason is Harbor Pilot's text (untrusted); SpaceMan never stores or shows it.
	Reason string `json:"reason"`
	// Until is when an unavailable account is expected back: the exhausted bucket's reset, or a cooldown's end. nil when
	// available, or when the provider gave no reset.
	Until     *time.Time `json:"until"`
	Remaining *float64   `json:"remaining"`
	ResetAt   *time.Time `json:"reset_at"`
	Stale     bool       `json:"stale"`
}

// QuotaBoard is the part of GET /api/v1/quota/board SpaceMan reads: account_health (monitors and observed accounts
// are not read).
type QuotaBoard struct {
	AccountHealth []AccountHealth `json:"account_health"`
}

// QuotaBoard reads the quota board (service-principal token with usage.read).
func (c *Client) QuotaBoard(ctx context.Context, serviceToken string) (*QuotaBoard, error) {
	var b QuotaBoard
	if err := c.do(ctx, "quota_board", http.MethodGet, "/api/v1/quota/board", serviceToken, nil, http.StatusOK, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// quotaBoard returns the quota board (cached for QuotaFresh).
func (b *Budgets) quotaBoard(ctx context.Context, r *reader) (*QuotaBoard, error) {
	v, _, err := load(ctx, b, "quota_board", r.key, slot[*QuotaBoard]{get: func() *cached[*QuotaBoard] { return b.board }, set: func(c *cached[*QuotaBoard]) { b.board = c }},
		b.o.QuotaFresh, func(ctx context.Context) (*QuotaBoard, error) {
			qb, err := r.c.QuotaBoard(ctx, r.token)
			if err != nil {
				return nil, statusErr("quota board", err)
			}
			return qb, nil
		})
	return v, err
}

// dropQuotaBoard forgets the cached quota board, so the next read asks Harbor Pilot.
func (b *Budgets) dropQuotaBoard() {
	b.mu.Lock()
	b.board = nil
	b.mu.Unlock()
}

// sameInstant compares two Harbor Pilot times to the second (the board's precision).
func sameInstant(a, b time.Time) bool { return a.Truncate(time.Second).Equal(b.Truncate(time.Second)) }

// EstimateQuota is a quota board's finding about the connection-wide quota hold h at now:
//   - recovered when no account is unavailable any more, or when h already has a board estimate and no unavailable
//     account is expected back at that time any more (the account it waited for recovered: its until passed or went);
//   - otherwise the estimate is the earliest future `until` among the unavailable accounts (known false when none has
//     one: the hold keeps its probe).
//
// Accounts that are available are ignored either way: the board lists every account of the installation, also ones
// the space's plan never routes to (a local model is always available).
func EstimateQuota(qb *QuotaBoard, h fleet.GatewayHold, now time.Time) (recovered bool, until time.Time, known bool) {
	unavailable := 0
	expected := false
	for _, a := range qb.AccountHealth {
		if a.Available {
			continue
		}
		unavailable++
		if a.Until == nil || !a.Until.After(now) {
			continue
		}
		if h.Estimated() && sameInstant(*a.Until, *h.ResetsAt) {
			expected = true
		}
		if !known || a.Until.Before(until) {
			until, known = a.Until.UTC(), true
		}
	}
	if unavailable == 0 || (h.Estimated() && h.Source == fleet.SourceQuotaBoard && !expected) {
		return true, time.Time{}, false
	}
	return false, until, known
}

// EstimateBudget is the covering budgets' finding about a project's budget hold: recovered when none blocks any more;
// otherwise the latest period end among those that block (known false when one has none: a one-time budget without an
// end never resets by itself).
func EstimateBudget(cov []Covering) (recovered bool, until time.Time, known bool) {
	blocking := 0
	for _, c := range cov {
		if !c.Blocking {
			continue
		}
		blocking++
		if c.PeriodEnd == nil {
			return false, time.Time{}, false
		}
		if c.PeriodEnd.After(until) {
			until = c.PeriodEnd.UTC()
		}
	}
	if blocking == 0 {
		return true, time.Time{}, false
	}
	return false, until, true
}

// GatewayHolds is what the gateway re-check needs from the fleet (implemented by *fleet.Service).
type GatewayHolds interface {
	OpenGatewayHolds(ctx context.Context) ([]fleet.GatewayHold, error)
	ApplyGatewayEstimate(ctx context.Context, h fleet.GatewayHold, e fleet.GatewayEstimate) (bool, error)
}

var _ GatewayHolds = (*fleet.Service)(nil)

const (
	// gatewayRetry is when a hold is read again after Harbor Pilot couldn't be read, or gave no estimate.
	gatewayRetry = 2 * time.Minute
	// gatewayReadTimeout bounds one read of the board or of a project's budgets.
	gatewayReadTimeout = 15 * time.Second
)

// GatewayWaits is the gateway re-check on central's deadline timer (Next and Due) for the gateway holds of every space.
type GatewayWaits struct {
	b     *Budgets
	holds GatewayHolds

	mu sync.Mutex
	// kicked makes every quota hold due now (quota.window.recovered).
	kicked bool
	// retry holds a hold back after a read that failed or estimated nothing, by hold id. Holds that are no longer open
	// drop out on the next round.
	retry map[string]time.Time
	wake  func()
}

// NewGatewayWaits returns the gateway re-check over b's connection (its reads and cache) and the fleet's holds. wake
// (optional) wakes the deadline timer after a Kick.
func NewGatewayWaits(b *Budgets, holds GatewayHolds, wake func()) *GatewayWaits {
	return &GatewayWaits{b: b, holds: holds, retry: map[string]time.Time{}, wake: wake}
}

// dueAt is when a hold is read next: a hold without Harbor Pilot's estimate yet at once, an estimated one at its
// check_at; never before a retry it is held back to; a kicked quota hold at once.
func (g *GatewayWaits) dueAt(h fleet.GatewayHold, now time.Time, kicked bool) time.Time {
	if kicked && h.Quota() {
		return now
	}
	at := now
	if h.Estimated() && h.CheckAt != nil {
		at = *h.CheckAt
	}
	if r, ok := g.retry[h.ID]; ok && r.After(at) {
		at = r
	}
	return at
}

// Next is when a gateway hold is next due for a read (zero: none is open).
func (g *GatewayWaits) Next(ctx context.Context) (time.Time, error) {
	holds, err := g.holds.OpenGatewayHolds(ctx)
	if err != nil || len(holds) == 0 {
		return time.Time{}, err
	}
	now := g.b.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	var next time.Time
	for _, h := range holds {
		if at := g.dueAt(h, now, g.kicked); next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next, nil
}

// Kick makes the quota holds due now and drops the cached board: Harbor Pilot reported a quota window recovered. The
// event is untrusted, and only brings the next read forward.
func (g *GatewayWaits) Kick() {
	g.b.dropQuotaBoard()
	g.mu.Lock()
	g.kicked = true
	g.mu.Unlock()
	if g.wake != nil {
		g.wake()
	}
}

// Due reads Harbor Pilot for the gateway holds that are due, outside any transaction (one board read for the quota
// hold of every space, one budget read per project), and applies each finding. It reads only the open holds when
// none is due, and writes nothing for a hold whose read failed. It returns how many holds it wrote.
func (g *GatewayWaits) Due(ctx context.Context) (int, error) {
	holds, err := g.holds.OpenGatewayHolds(ctx)
	if err != nil {
		return 0, err
	}
	now := g.b.now()
	g.mu.Lock()
	kicked := g.kicked
	open := make(map[string]bool, len(holds))
	var due []fleet.GatewayHold
	for _, h := range holds {
		open[h.ID] = true
		if !g.dueAt(h, now, kicked).After(now) {
			due = append(due, h)
		}
	}
	for id := range g.retry {
		if !open[id] {
			delete(g.retry, id)
		}
	}
	g.mu.Unlock()
	if len(due) == 0 {
		g.mu.Lock()
		g.kicked = false
		g.mu.Unlock()
		return 0, nil
	}
	r, rerr := g.b.reader(ctx)
	var (
		board    *QuotaBoard
		boardErr error
		boardRd  bool
		budgets  = map[string][]Covering{}
		budErrs  = map[string]error{}
		done     int
		errs     []error
	)
	for _, h := range due {
		var e fleet.GatewayEstimate
		var recovered, known bool
		var until time.Time
		var readErr error
		switch {
		case rerr != nil:
			readErr = rerr
		case h.Quota():
			if !boardRd {
				rctx, cancel := context.WithTimeout(ctx, gatewayReadTimeout)
				board, boardErr = g.b.quotaBoard(rctx, r)
				cancel()
				boardRd = true
				g.mu.Lock()
				g.kicked = false
				g.mu.Unlock()
			}
			if readErr = boardErr; readErr == nil {
				recovered, until, known = EstimateQuota(board, h, now)
				e.Source = fleet.SourceQuotaBoard
			}
		case h.ProjectID != "":
			cov, seen := budgets[h.ProjectID]
			if !seen && budErrs[h.ProjectID] == nil {
				rctx, cancel := context.WithTimeout(ctx, gatewayReadTimeout)
				cov, _, _, readErr = g.b.covering(rctx, h.ProjectID)
				cancel()
				if readErr != nil {
					budErrs[h.ProjectID] = readErr
				} else {
					budgets[h.ProjectID] = cov
				}
			}
			if readErr == nil {
				readErr = budErrs[h.ProjectID]
			}
			if readErr == nil {
				recovered, until, known = EstimateBudget(cov)
				e.Source = fleet.SourceBudgetStatus
			}
		default:
			// A gateway reference this version doesn't estimate: its probe stays in charge.
			g.holdBack(h.ID, now.Add(fleet.GatewayRecheck))
			continue
		}
		if readErr != nil {
			g.readFailed(h, readErr)
			retry := gatewayRetry
			if rerr != nil {
				// No usable connection or service token: nothing changes that soon.
				retry = fleet.GatewayRecheck
			}
			g.holdBack(h.ID, now.Add(retry))
			continue
		}
		if !recovered && !known {
			// Exhausted, but Harbor Pilot names no reset: the hold keeps its probe (decision 7), read again later.
			g.holdBack(h.ID, now.Add(fleet.GatewayRecheck))
			continue
		}
		e.Recovered, e.ResetsAt = recovered, until
		wrote, err := g.holds.ApplyGatewayEstimate(ctx, h, e)
		if err != nil {
			errs = append(errs, err)
			g.holdBack(h.ID, now.Add(gatewayRetry))
			continue
		}
		g.mu.Lock()
		delete(g.retry, h.ID)
		g.mu.Unlock()
		if wrote {
			done++
		}
	}
	return done, errors.Join(errs...)
}

func (g *GatewayWaits) holdBack(id string, at time.Time) {
	g.mu.Lock()
	g.retry[id] = at
	g.mu.Unlock()
}

// readFailed logs why a hold couldn't be estimated: a fixed reason, never Harbor Pilot's body or a token.
func (g *GatewayWaits) readFailed(h fleet.GatewayHold, err error) {
	var ue *errStatusUnavailable
	if errors.As(err, &ue) {
		g.b.svc.log().Warn("harbor pilot gateway hold: not estimated", slog.String("usage_limit.id", h.ID), slog.String("reason", ue.reason))
		return
	}
	g.b.svc.log().Warn("harbor pilot gateway hold: not estimated", slog.String("usage_limit.id", h.ID), "err", err)
}
