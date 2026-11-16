package usage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// The usage ledger (ADR-0090, plan T6): the growth of a run's cumulative counters goes into hourly buckets per space,
// account, role, harness and model. The run keeps the highest counters it has already counted (run_usage.accounted_json),
// so a repeated report adds nothing, a stale one that arrives after a newer one adds nothing, and the final report adds
// only what the earlier ones missed. Counters never go down in the ledger. Unknown (absent) counters count as zero.

const (
	// GranHour and GranDay are the bucket granularities.
	GranHour = "hour"
	GranDay  = "day"

	// UnknownModel is the bucket's model when a report's model id is not a plausible identifier.
	UnknownModel = "unknown"

	// maxCounter bounds one cumulative counter (as the status line's intake does); a larger one is clamped.
	maxCounter = int64(1) << 40
	// maxCost bounds a cumulative cost estimate in USD.
	maxCost = 1e7

	// WindowSampleSlot is the spacing of account window samples (at most one per account and kind in a slot).
	WindowSampleSlot = 10 * time.Minute
	maxWindows       = 8
)

// reModel is what a model id looks like (claude-sonnet-4-5, a dated id, a bracketed context suffix); anything else is
// stored as UnknownModel, never as free text.
var reModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,127}$`)

// counters are the cumulative token counters and request count of one model.
type counters struct {
	Model      string `json:"model"`
	Input      int64  `json:"in"`
	Output     int64  `json:"out"`
	CacheWrite int64  `json:"cw"`
	CacheRead  int64  `json:"cr"`
	Requests   int64  `json:"req"`
}

func (c counters) zero() bool {
	return c.Input == 0 && c.Output == 0 && c.CacheWrite == 0 && c.CacheRead == 0 && c.Requests == 0
}

func clampCounter(p *int64) int64 {
	if p == nil || *p < 0 {
		return 0
	}
	return min(*p, maxCounter)
}

// modelCounters folds a report's models into one counters value per validated model id (a model named twice keeps the
// higher counters, so a repeated name cannot add up to more than the node counted).
func modelCounters(models []proto.ModelUsage) map[string]counters {
	out := map[string]counters{}
	for _, m := range models {
		name := m.Model
		if !reModel.MatchString(name) {
			name = UnknownModel
		}
		c := counters{Model: name, Input: clampCounter(m.Input), Output: clampCounter(m.Output), CacheWrite: clampCounter(m.CacheWrite),
			CacheRead: clampCounter(m.CacheRead), Requests: min(max(m.Requests, 0), maxCounter)}
		out[name] = maxCounters(out[name], c)
	}
	return out
}

func maxCounters(a, b counters) counters {
	return counters{Model: b.Model, Input: max(a.Input, b.Input), Output: max(a.Output, b.Output), CacheWrite: max(a.CacheWrite, b.CacheWrite),
		CacheRead: max(a.CacheRead, b.CacheRead), Requests: max(a.Requests, b.Requests)}
}

// Delta is what one report adds to the ledger.
type Delta struct {
	// Models are the growth per model (only models that grew), sorted by model.
	Models []counters
	// Cost is the growth of the cost estimate in USD.
	Cost float64
	// Ledger and LedgerCost are the new high-water marks to store.
	Ledger     []counters
	LedgerCost float64
}

// computeDelta returns the growth of the report's cumulative counters over what the run has already counted. A counter
// that is lower than the counted one (a stale or repeated report) contributes nothing and does not lower the stored mark.
func computeDelta(counted []counters, countedCost float64, models []proto.ModelUsage, est *proto.CostEstimate) Delta {
	prev := map[string]counters{}
	for _, c := range counted {
		prev[c.Model] = c
	}
	next := map[string]counters{}
	for k, v := range prev {
		next[k] = v
	}
	var d Delta
	for name, cur := range modelCounters(models) {
		p := prev[name]
		n := maxCounters(p, cur)
		n.Model = name
		next[name] = n
		g := counters{Model: name, Input: n.Input - p.Input, Output: n.Output - p.Output, CacheWrite: n.CacheWrite - p.CacheWrite,
			CacheRead: n.CacheRead - p.CacheRead, Requests: n.Requests - p.Requests}
		if !g.zero() {
			d.Models = append(d.Models, g)
		}
	}
	sort.Slice(d.Models, func(i, j int) bool { return d.Models[i].Model < d.Models[j].Model })
	d.LedgerCost = countedCost
	if est != nil && !math.IsNaN(est.USD) && !math.IsInf(est.USD, 0) && est.USD > countedCost {
		d.LedgerCost = min(est.USD, maxCost)
		d.Cost = max(d.LedgerCost-countedCost, 0)
		d.LedgerCost = countedCost + d.Cost
	}
	for _, v := range next {
		d.Ledger = append(d.Ledger, v)
	}
	sort.Slice(d.Ledger, func(i, j int) bool { return d.Ledger[i].Model < d.Ledger[j].Model })
	return d
}

// costModel picks the model the report's cost growth is booked under: the one whose tokens grew the most (the estimate
// is one figure for the run, not per model). UnknownModel when no model grew.
func (d Delta) costModel() string {
	best, bestN := UnknownModel, int64(-1)
	for _, m := range d.Models {
		if n := m.Input + m.Output + m.CacheWrite + m.CacheRead; n > bestN {
			best, bestN = m.Model, n
		}
	}
	return best
}

// HourStart is the start of the UTC hour containing t, as stored (Unix microseconds).
func HourStart(t time.Time) int64 { return t.UTC().Truncate(time.Hour).UnixMicro() }

// runKey is who a run's usage is booked for.
type runKey struct{ space, account, role, harness string }

// accountRef is the one field of a run's launch spec the ledger reads.
type launchAccount struct {
	AccountRef string `json:"account_ref"`
}

// markKey is what a run's counted counters are filed under: its harness session (a resumed run continues the same
// transcript, so its cumulative counters start where the earlier run's ended), or the run itself without a usable one.
func markKey(r db.Run) string {
	if reSession.MatchString(r.SessionID) {
		return r.SessionID
	}
	return "run:" + r.ID
}

var reSession = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func runAccount(r db.Run) string {
	var la launchAccount
	if json.Unmarshal([]byte(r.LaunchJson), &la) != nil {
		return ""
	}
	return la.AccountRef
}

// account books a kept report in the ledger, inside the report's transaction: the growth over what the run's session
// has already had counted goes into the current hour's buckets, and the run's windows into the account's.
func (svc *Service) account(ctx context.Context, tx *store.Tx, r db.Run, rep proto.UsageReport) error {
	mark, err := tx.Q.GetUsageLedgerMark(ctx, db.GetUsageLedgerMarkParams{SpaceID: r.SpaceID, MarkKey: markKey(r)})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var counted []counters
	if mark.CountedJson != "" {
		_ = json.Unmarshal([]byte(mark.CountedJson), &counted)
	}
	d := computeDelta(counted, mark.CountedCost, rep.Models, rep.Estimate)
	key := runKey{space: r.SpaceID, account: runAccount(r), role: r.Role, harness: r.Harness}
	start := HourStart(tx.Now)
	costModel := d.costModel()
	for _, m := range d.Models {
		p := db.AddUsageBucketParams{SpaceID: key.space, AccountID: key.account, Role: key.role, Harness: key.harness, Model: m.Model, Granularity: GranHour,
			BucketStart: start, InputTokens: m.Input, OutputTokens: m.Output, CacheWriteTokens: m.CacheWrite, CacheReadTokens: m.CacheRead, Requests: m.Requests}
		if m.Model == costModel {
			p.CostUsd, d.Cost = d.Cost, 0
		}
		if err := tx.Q.AddUsageBucket(ctx, p); err != nil {
			return err
		}
	}
	if d.Cost > 0 {
		if err := tx.Q.AddUsageBucket(ctx, db.AddUsageBucketParams{SpaceID: key.space, AccountID: key.account, Role: key.role, Harness: key.harness,
			Model: costModel, Granularity: GranHour, BucketStart: start, CostUsd: d.Cost}); err != nil {
			return err
		}
	}
	ledger, _ := json.Marshal(nonNilCounters(d.Ledger))
	if err := tx.Q.SetUsageLedgerMark(ctx, db.SetUsageLedgerMarkParams{SpaceID: r.SpaceID, MarkKey: markKey(r), CountedJson: string(ledger), CountedCost: d.LedgerCost,
		UpdatedAt: tx.NowMicros()}); err != nil {
		return err
	}
	return svc.recordWindows(ctx, tx, r, key.account, rep.Windows)
}

func nonNilCounters(c []counters) []counters {
	if c == nil {
		return []counters{}
	}
	return c
}

// validWindow checks a reported window again at central: a known kind, a finite non-negative percentage (clamped to
// 100), an observation not from the future (beyond the skew) or older than the limit window, and a reset inside the
// limits' own bounds. A reset outside them is dropped, not the window.
func validWindow(w proto.UsageWindow, now time.Time) (proto.UsageWindow, bool) {
	if w.Kind != proto.WindowFiveHour && w.Kind != proto.WindowSevenDay {
		return w, false
	}
	if math.IsNaN(w.UsedPct) || math.IsInf(w.UsedPct, 0) || w.UsedPct < 0 {
		return w, false
	}
	w.UsedPct = min(w.UsedPct, 100)
	switch {
	case w.ObservedAt.IsZero(), w.ObservedAt.After(now.Add(proto.MaxLimitSkew)), w.ObservedAt.Before(now.Add(-proto.MaxLimitReset)):
		return w, false
	}
	w.ObservedAt = w.ObservedAt.UTC()
	if w.ObservedAt.After(now) {
		w.ObservedAt = now.UTC()
	}
	if r := w.ResetsAt; r != nil {
		if r.After(w.ObservedAt) && !r.After(now.Add(proto.MaxLimitReset)) {
			v := r.UTC()
			w.ResetsAt = &v
		} else {
			w.ResetsAt = nil
		}
	}
	return w, true
}

// recordWindows keeps the validated windows of a run's account: the latest per kind (newest observation wins) and a
// sample per ten-minute slot. A run without a subscription account, or whose account no longer exists, keeps none.
func (svc *Service) recordWindows(ctx context.Context, tx *store.Tx, r db.Run, accountID string, windows []proto.UsageWindow) error {
	if accountID == "" || len(windows) == 0 || len(windows) > maxWindows {
		return nil
	}
	if _, err := tx.Q.GetAccount(ctx, accountID); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, w := range windows {
		w, ok := validWindow(w, tx.Now)
		if !ok || seen[w.Kind] {
			continue
		}
		seen[w.Kind] = true
		var resets sql.NullInt64
		if w.ResetsAt != nil {
			resets = sql.NullInt64{Int64: w.ResetsAt.UnixMicro(), Valid: true}
		}
		obs := w.ObservedAt.UnixMicro()
		if err := tx.Q.UpsertAccountWindow(ctx, db.UpsertAccountWindowParams{AccountID: accountID, Kind: w.Kind, UsedPct: w.UsedPct, ResetsAt: resets,
			ObservedAt: obs, RunID: r.ID, UpdatedAt: tx.NowMicros()}); err != nil {
			return err
		}
		if err := tx.Q.UpsertAccountWindowSample(ctx, db.UpsertAccountWindowSampleParams{AccountID: accountID, Kind: w.Kind, Slot: obs / WindowSampleSlot.Microseconds(),
			UsedPct: w.UsedPct, ResetsAt: resets, ObservedAt: obs, RunID: r.ID}); err != nil {
			return err
		}
	}
	return nil
}
