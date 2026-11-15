package harborpilot

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
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
// Harbor Pilot's quota.window.recovered event arrives (Kick). A quota hold clears when one of the accounts behind its
// estimate is available again (or none is unavailable), a budget hold when no budget blocks any more, which wakes the
// runs parked under it. An empty board, an unknown reset or a passed period end is no estimate: the hold keeps its probe.

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

// quotaBoard returns the quota board (cached for QuotaFresh). A read that started before the cached board was dropped
// (a Kick racing it) is neither cached nor returned: the board is read again.
func (b *Budgets) quotaBoard(ctx context.Context, r *reader) (*QuotaBoard, error) {
	for {
		b.mu.Lock()
		gen := b.boardGen
		b.mu.Unlock()
		v, _, err := load(ctx, b, "quota_board", r.key, slot[*QuotaBoard]{
			get: func() *cached[*QuotaBoard] { return b.board },
			// set runs under b.mu.
			set: func(c *cached[*QuotaBoard]) {
				if b.boardGen == gen {
					b.board = c
				}
			}},
			b.o.QuotaFresh, func(ctx context.Context) (*QuotaBoard, error) {
				qb, err := r.c.QuotaBoard(ctx, r.token)
				if err != nil {
					return nil, statusErr("quota board", err)
				}
				return qb, nil
			})
		b.mu.Lock()
		dropped := b.boardGen != gen
		b.mu.Unlock()
		if !dropped || ctx.Err() != nil {
			return v, err
		}
	}
}

// dropQuotaBoard forgets the cached quota board, so the next read asks Harbor Pilot, and keeps a read already in
// flight from being cached.
func (b *Budgets) dropQuotaBoard() {
	b.mu.Lock()
	b.board = nil
	b.boardGen++
	b.mu.Unlock()
}

// sameInstant compares two Harbor Pilot times to the second (the board's precision).
func sameInstant(a, b time.Time) bool { return a.Truncate(time.Second).Equal(b.Truncate(time.Second)) }

// Estimate is what a read of Harbor Pilot found about a gateway hold.
type Estimate struct {
	// Recovered clears the hold.
	Recovered bool
	// Until is the estimated reset when Known.
	Until time.Time
	Known bool
	// Soon asks for the next read after the short retry rather than the 15-minute re-check, when there is no estimate
	// for now (a blocking budget's period end has passed: the new period should start any moment).
	Soon bool
	// Accounts are the unavailable accounts whose until is the estimate (quota): the hold clears when one of them is
	// available again.
	Accounts []string
}

// EstimateQuota is a quota board's finding about the connection-wide quota hold at now. expected are the accounts
// behind the hold's current board estimate (nil when there is none, or when central doesn't know them any more):
//   - no estimate when the board lists no account at all: an empty, malformed or renamed account_health is no
//     evidence, so the hold keeps its probe;
//   - recovered when no account is unavailable any more, or when one of the expected accounts is available again;
//   - otherwise the estimate is the earliest future `until` among the unavailable accounts, with the accounts behind it
//     (known false when none has one: the hold keeps its probe). An expected account that is still out with a moved
//     `until` re-times the hold; it never clears it.
//
// Accounts that are available are otherwise ignored: the board lists every account of the installation, also ones the
// space's plan never routes to (a local model is always available).
func EstimateQuota(qb *QuotaBoard, expected []string, now time.Time) Estimate {
	var e Estimate
	if qb == nil || len(qb.AccountHealth) == 0 {
		return e
	}
	unavailable := 0
	back := false
	for _, a := range qb.AccountHealth {
		if a.Available {
			back = back || (a.AccountID != "" && slices.Contains(expected, a.AccountID))
			continue
		}
		unavailable++
		if a.Until == nil || !a.Until.After(now) {
			continue
		}
		if !e.Known || a.Until.Before(e.Until) {
			e.Until, e.Known = a.Until.UTC(), true
		}
	}
	if unavailable == 0 || back {
		return Estimate{Recovered: true}
	}
	if e.Known {
		for _, a := range qb.AccountHealth {
			if !a.Available && a.Until != nil && sameInstant(*a.Until, e.Until) && !slices.Contains(e.Accounts, a.AccountID) {
				e.Accounts = append(e.Accounts, a.AccountID)
			}
		}
	}
	return e
}

// EstimateBudget is the covering budgets' finding about a project's budget hold at now: recovered when none blocks
// any more; otherwise the latest period end among those that block. There is no estimate when one has no end (a
// one-time budget never resets by itself), or when a blocking budget's period end has passed (Soon: Harbor Pilot
// hasn't rolled the period yet, so the hold keeps its probe and is read again shortly).
func EstimateBudget(cov []Covering, now time.Time) Estimate {
	var e Estimate
	blocking, passed := 0, false
	for _, c := range cov {
		if !c.Blocking {
			continue
		}
		blocking++
		if c.PeriodEnd == nil {
			return Estimate{}
		}
		if !c.PeriodEnd.After(now) {
			passed = true
			continue
		}
		if c.PeriodEnd.After(e.Until) {
			e.Until = c.PeriodEnd.UTC()
		}
	}
	switch {
	case blocking == 0:
		return Estimate{Recovered: true}
	case passed:
		return Estimate{Soon: true}
	}
	e.Known = true
	return e
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
	// kicked makes every quota hold due now (quota.window.recovered); gen counts the kicks, so a round clears kicked
	// only when no kick came while it read.
	kicked bool
	gen    uint64
	// retry holds a hold back, by hold id, after a read that failed, estimated nothing or found the estimate the hold
	// already has. It applies only to the hold's version it was set for: a hold that changed meanwhile is read at its
	// own time.
	retry map[string]heldBack
	// expected are the accounts behind a quota hold's board estimate, by hold id, for the hold's version they were
	// read for (in memory only: after a restart the next read finds them again).
	expected map[string]heldAccounts
	wake     func()
}

type heldBack struct {
	at      time.Time
	version int64
}

type heldAccounts struct {
	ids     []string
	version int64
}

// NewGatewayWaits returns the gateway re-check over b's connection (its reads and cache) and the fleet's holds. wake
// (optional) wakes the deadline timer after a Kick.
func NewGatewayWaits(b *Budgets, holds GatewayHolds, wake func()) *GatewayWaits {
	return &GatewayWaits{b: b, holds: holds, retry: map[string]heldBack{}, expected: map[string]heldAccounts{}, wake: wake}
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
	if r, ok := g.retry[h.ID]; ok && r.version == h.Version && r.at.After(at) {
		at = r.at
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
	g.gen++
	g.mu.Unlock()
	if g.wake != nil {
		g.wake()
	}
}

// Due reads Harbor Pilot for the gateway holds that are due, outside any transaction (one board read for the quota
// hold of every space, one budget read per project), and applies each finding. It reads only the open holds when
// none is due, and writes nothing for a hold whose read failed or whose estimate is unchanged. It returns how many
// holds it wrote.
func (g *GatewayWaits) Due(ctx context.Context) (int, error) {
	holds, err := g.holds.OpenGatewayHolds(ctx)
	if err != nil {
		return 0, err
	}
	now := g.b.now()
	g.mu.Lock()
	// A kick with no quota hold open stays set: it makes nothing due, and the first board read clears it.
	kicked, gen := g.kicked, g.gen
	open := make(map[string]int64, len(holds))
	var due []fleet.GatewayHold
	for _, h := range holds {
		open[h.ID] = h.Version
		if !g.dueAt(h, now, kicked).After(now) {
			due = append(due, h)
		}
	}
	for id, r := range g.retry {
		if v, ok := open[id]; !ok || v != r.version {
			delete(g.retry, id)
		}
	}
	for id, x := range g.expected {
		if v, ok := open[id]; !ok || v != x.version {
			delete(g.expected, id)
		}
	}
	g.mu.Unlock()
	if len(due) == 0 {
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
		var est Estimate
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
				if g.gen == gen {
					g.kicked = false
				}
				g.mu.Unlock()
			}
			if readErr = boardErr; readErr == nil {
				est = EstimateQuota(board, g.expectedFor(h), now)
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
				est = EstimateBudget(cov, now)
				e.Source = fleet.SourceBudgetStatus
			}
		default:
			// A gateway reference this version doesn't estimate: its probe stays in charge.
			g.holdBack(h, now.Add(fleet.GatewayRecheck))
			continue
		}
		if readErr != nil {
			g.readFailed(h, readErr)
			retry := gatewayRetry
			if rerr != nil {
				// No usable connection or service token: nothing changes that soon.
				retry = fleet.GatewayRecheck
			}
			g.holdBack(h, now.Add(retry))
			continue
		}
		if !est.Recovered && !est.Known {
			// Exhausted, but Harbor Pilot names no reset (or the board lists nothing): the hold keeps its probe
			// (decision 7), read again later.
			retry := fleet.GatewayRecheck
			if est.Soon {
				retry = gatewayRetry
			}
			g.holdBack(h, now.Add(retry))
			continue
		}
		e.Recovered, e.ResetsAt = est.Recovered, est.Until
		wrote, err := g.holds.ApplyGatewayEstimate(ctx, h, e)
		if err != nil {
			errs = append(errs, err)
			g.holdBack(h, now.Add(gatewayRetry))
			continue
		}
		version := h.Version
		if wrote {
			// The fleet moves an estimated hold's version on by one (SetUsageLimitEstimate).
			version++
			done++
		}
		g.mu.Lock()
		if e.Recovered {
			delete(g.retry, h.ID)
			delete(g.expected, h.ID)
		} else {
			if e.Source == fleet.SourceQuotaBoard {
				g.expected[h.ID] = heldAccounts{ids: est.Accounts, version: version}
			}
			if wrote {
				// check_at is the next re-read.
				delete(g.retry, h.ID)
			} else {
				// Unchanged (or changed meanwhile, when the hold-back doesn't apply): the next re-read is kept here.
				g.retry[h.ID] = heldBack{at: now.Add(fleet.GatewayRecheck), version: h.Version}
			}
		}
		g.mu.Unlock()
	}
	return done, errors.Join(errs...)
}

// expectedFor returns the accounts behind h's board estimate, when central still knows them for h's version.
func (g *GatewayWaits) expectedFor(h fleet.GatewayHold) []string {
	if !h.Estimated() || h.Source != fleet.SourceQuotaBoard {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if x, ok := g.expected[h.ID]; ok && x.version == h.Version {
		return x.ids
	}
	return nil
}

func (g *GatewayWaits) holdBack(h fleet.GatewayHold, at time.Time) {
	g.mu.Lock()
	g.retry[h.ID] = heldBack{at: at, version: h.Version}
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
