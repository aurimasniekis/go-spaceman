package harborpilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/harborpilot/hpfake"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func readBoard(t *testing.T, name string) *QuotaBoard {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/quota/board" || r.Header.Get("Authorization") != "Bearer hpa_tok" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	qb, err := c.QuotaBoard(context.Background(), "hpa_tok")
	if err != nil {
		t.Fatal(err)
	}
	return qb
}

// The fixtures are GET /api/v1/quota/board from a real `harbor-pilot serve --demo` v0.1.6 (provisioned by hptest),
// recorded on 2026-11-14 with every quota monitor's exhausted_below at 0.99 (exhausted) and back at 0 (recovered). The
// demo's local model account has no monitor and stays available throughout.
func TestQuotaBoardFixtureDecode(t *testing.T) {
	t.Parallel()
	ex := readBoard(t, "quota_board_v016_exhausted.json")
	if len(ex.AccountHealth) != 4 {
		t.Fatalf("account_health: %+v", ex.AccountHealth)
	}
	byName := map[string]AccountHealth{}
	for _, a := range ex.AccountHealth {
		byName[a.Name] = a
	}
	team2 := byName["anthropic-team-2"]
	if team2.Available || team2.Until == nil || !team2.Until.Equal(mustTime(t, "2026-11-14T05:30:00Z")) || team2.Billing != "metered_api" ||
		team2.Remaining == nil || team2.ResetAt == nil || team2.Stale || team2.AccountID == "" || team2.Reason == "" {
		t.Fatalf("anthropic-team-2: %+v", team2)
	}
	if gpu := byName["workstation-gpu"]; !gpu.Available || gpu.Until != nil || gpu.Remaining != nil || gpu.Billing != "local" {
		t.Fatalf("workstation-gpu: %+v", gpu)
	}
	// The estimate is the earliest until among the unavailable accounts, whatever the available local model says.
	now := mustTime(t, "2026-11-14T01:51:00Z")
	e := EstimateQuota(ex, nil, now)
	if e.Recovered || !e.Known || !e.Until.Equal(mustTime(t, "2026-11-14T05:30:00Z")) || !slices.Equal(e.Accounts, []string{team2.AccountID}) {
		t.Fatalf("estimate: %+v", e)
	}

	rec := readBoard(t, "quota_board_v016_recovered.json")
	for _, a := range rec.AccountHealth {
		if !a.Available || a.Until != nil {
			t.Fatalf("recovered board: %+v", a)
		}
	}
	if e := EstimateQuota(rec, e.Accounts, now); !e.Recovered {
		t.Fatal("the recovered board doesn't recover the hold")
	}
}

func board(accounts ...AccountHealth) *QuotaBoard { return &QuotaBoard{AccountHealth: accounts} }

func down(name string, until *time.Time) AccountHealth {
	return AccountHealth{AccountID: "acc_" + name, Name: name, Until: until}
}

func up(name string) AccountHealth {
	return AccountHealth{AccountID: "acc_" + name, Name: name, Available: true}
}

func TestEstimateQuota(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-11-14T10:00:00Z")
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	for _, c := range []struct {
		name string
		// expected are the accounts behind the hold's current board estimate (nil: none, or not known).
		expected  []string
		b         *QuotaBoard
		recovered bool
		until     *time.Time
		accounts  []string
	}{
		{"earliest until wins", nil, board(down("a", at(3*time.Hour)), down("b", at(time.Hour)), up("local")), false, at(time.Hour), []string{"acc_b"}},
		{"every account at the earliest until is behind it", nil, board(down("a", at(time.Hour)), down("b", at(time.Hour+time.Millisecond))), false, at(time.Hour), []string{"acc_a", "acc_b"}},
		{"past and unknown untils are no estimate", nil, board(down("a", at(-time.Minute)), down("b", nil)), false, nil, nil},
		{"nothing unavailable recovers", nil, board(up("a"), up("b")), true, nil, nil},
		{"an empty board is no estimate", nil, board(), false, nil, nil},
		{"a board without account_health is no estimate", []string{"acc_b"}, &QuotaBoard{}, false, nil, nil},
		{"the expected account is still out", []string{"acc_b"}, board(down("a", at(3*time.Hour)), down("b", at(time.Hour))), false, at(time.Hour), []string{"acc_b"}},
		{"the expected account came back", []string{"acc_b"}, board(down("a", at(3*time.Hour)), up("b")), true, nil, nil},
		{"another account coming back doesn't recover", []string{"acc_b"}, board(down("b", at(time.Hour)), up("a")), false, at(time.Hour), []string{"acc_b"}},
		{"the expected account moved its until", []string{"acc_b"}, board(down("a", at(3*time.Hour)), down("b", at(2*time.Hour))), false, at(2 * time.Hour), []string{"acc_b"}},
		{"the expected reset passed, the account still out", []string{"acc_b"}, board(down("a", at(3*time.Hour)), down("b", nil)), false, at(3 * time.Hour), []string{"acc_a"}},
		{"unknown expected accounts never recover a moved until", nil, board(down("a", at(3*time.Hour)), up("b")), false, at(3 * time.Hour), []string{"acc_a"}},
		{"a newly exhausted account moves the estimate earlier", []string{"acc_a"}, board(down("a", at(time.Hour)), down("b", at(10*time.Minute))), false, at(10 * time.Minute), []string{"acc_b"}},
		{"sub-second jitter is the same instant", []string{"acc_a"}, board(down("a", at(time.Hour+300*time.Millisecond))), false, at(time.Hour + 300*time.Millisecond), []string{"acc_a"}},
	} {
		e := EstimateQuota(c.b, c.expected, now)
		if e.Recovered != c.recovered || e.Known != (c.until != nil) || (c.until != nil && !e.Until.Equal(*c.until)) || !slices.Equal(e.Accounts, c.accounts) || e.Soon {
			t.Errorf("%s: got %+v, want %v %v %v", c.name, e, c.recovered, c.until, c.accounts)
		}
	}
	// An empty or renamed account_health decodes to no accounts: no estimate, never a recovery.
	for _, body := range []string{`{}`, `{"account_health":null}`, `{"account_health":[]}`, `{"accounts":[{"account_id":"acc_a","available":true}]}`} {
		var qb QuotaBoard
		if err := json.Unmarshal([]byte(body), &qb); err != nil {
			t.Fatal(err)
		}
		if e := EstimateQuota(&qb, []string{"acc_a"}, now); e.Recovered || e.Known {
			t.Errorf("%s: %+v", body, e)
		}
	}
}

func TestEstimateBudget(t *testing.T) {
	t.Parallel()
	now := mustTime(t, "2026-11-14T10:00:00Z")
	end1, end2, past := mustTime(t, "2026-11-15T00:00:00Z"), mustTime(t, "2026-12-01T00:00:00Z"), mustTime(t, "2026-11-14T00:00:00Z")
	blocking := func(end *time.Time) Covering { return Covering{Blocking: true, State: BudgetExceeded, PeriodEnd: end} }
	if e := EstimateBudget([]Covering{{State: BudgetWarning}, {State: BudgetExceeded}}, now); !e.Recovered {
		t.Fatal("no blocking budget must recover")
	}
	if e := EstimateBudget([]Covering{blocking(&end2), blocking(&end1), {State: BudgetOK}}, now); e.Recovered || !e.Known || !e.Until.Equal(end2) || e.Soon {
		t.Fatalf("latest period end of the blocking budgets: %+v", e)
	}
	if e := EstimateBudget([]Covering{blocking(&end1), blocking(nil)}, now); e.Recovered || e.Known || e.Soon {
		t.Fatalf("a blocking one-time budget without an end has no estimate: %+v", e)
	}
	// A period end that passed (or is now) is no estimate: Harbor Pilot hasn't rolled the period yet; read again soon.
	for _, end := range []time.Time{past, now} {
		if e := EstimateBudget([]Covering{blocking(&end2), blocking(&end)}, now); e.Recovered || e.Known || !e.Soon {
			t.Fatalf("a passed period end %v: %+v", end, e)
		}
	}
}

// fakeHolds is the fleet side of the gateway re-check.
type fakeHolds struct {
	mu      sync.Mutex
	holds   []fleet.GatewayHold
	applied []fleet.GatewayEstimate
	ids     []string
	writes  int
	now     func() time.Time
}

func (f *fakeHolds) OpenGatewayHolds(context.Context) ([]fleet.GatewayHold, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.holds), nil
}

func (f *fakeHolds) ApplyGatewayEstimate(_ context.Context, h fleet.GatewayHold, e fleet.GatewayEstimate) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied, f.ids = append(f.applied, e), append(f.ids, h.ID)
	for i := range f.holds {
		if f.holds[i].ID != h.ID || f.holds[i].Version != h.Version {
			continue
		}
		if e.Recovered {
			f.holds = slices.Delete(f.holds, i, i+1)
			return true, nil
		}
		if f.holds[i].ResetsAt != nil && f.holds[i].ResetsAt.Equal(e.ResetsAt) && f.holds[i].Source == e.Source {
			return false, nil // unchanged: nothing written
		}
		r, c := e.ResetsAt, f.now().Add(fleet.GatewayRecheck)
		f.holds[i].ResetsAt, f.holds[i].Source, f.holds[i].CheckAt = &r, e.Source, &c
		f.holds[i].Version++
		f.writes++
		return true, nil
	}
	return false, nil
}

func (f *fakeHolds) take() ([]string, []fleet.GatewayEstimate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids, ap := f.ids, f.applied
	f.ids, f.applied = nil, nil
	return ids, ap
}

// budgetEnding sets a budgetJSON's period end.
func budgetEnding(b string, end time.Time) string {
	return strings.Replace(b, `"period_end":"2026-11-09T00:00:00Z"`, `"period_end":"`+end.Format(time.RFC3339)+`"`, 1)
}

func boardJSON(t *testing.T, b *QuotaBoard) string {
	t.Helper()
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The re-check reads the board once for the quota holds of every space and once per project for budget holds, outside
// any transaction; a hold Harbor Pilot can't estimate is held back instead of read again on every tick; a kick drops
// the cached board and makes the quota holds due at once.
func TestGatewayWaitsDue(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	b := newBudgets(svc)
	now := mustTime(t, "2026-11-14T10:00:00Z")
	var clk sync.Mutex
	b.now = func() time.Time { clk.Lock(); defer clk.Unlock(); return now }
	advance := func(d time.Duration) { clk.Lock(); now = now.Add(d); clk.Unlock() }
	ctx := context.Background()
	reset, periodEnd := now.Add(90*time.Minute), mustTime(t, "2026-11-15T00:00:00Z")
	f.Set(func(h *hpfake.HP) {
		h.Board = boardJSON(t, board(down("a", &reset), up("local")))
		h.Budgets = items(budgetEnding(budgetJSON("bdg_1", "daily", ScopeProject, "prj_web", EnforceBlock, BudgetExceeded, true, 1e9, 12e8), periodEnd))
	})
	holds := &fakeHolds{holds: []fleet.GatewayHold{
		{ID: "ulm_q1", SpaceID: "spc_a", Ref: "harbor-pilot", Kind: "gateway_quota", Source: "transcript"},
		{ID: "ulm_q2", SpaceID: "spc_b", Ref: "harbor-pilot", Kind: "gateway_quota", Source: "transcript"},
		{ID: "ulm_b1", SpaceID: "spc_a", Ref: "harbor-pilot:project:prj_web", Kind: "gateway_budget", ProjectID: "prj_web", Source: "transcript"},
	}}
	holds.now = b.now
	woke := 0
	g := NewGatewayWaits(b, holds, func() { woke++ })

	if at, err := g.Next(ctx); err != nil || !at.Equal(now) {
		t.Fatalf("new holds are due at once: %v %v", at, err)
	}
	n, err := g.Due(ctx)
	if err != nil || n != 3 {
		t.Fatalf("due: %d %v", n, err)
	}
	ids, ap := holds.take()
	if !slices.Equal(ids, []string{"ulm_q1", "ulm_q2", "ulm_b1"}) || !ap[0].ResetsAt.Equal(reset) || ap[0].Source != fleet.SourceQuotaBoard ||
		!ap[2].ResetsAt.Equal(periodEnd) || ap[2].Source != fleet.SourceBudgetStatus {
		t.Fatalf("applied %v %+v", ids, ap)
	}
	reads := func() (board, budgets int) {
		f.Set(func(h *hpfake.HP) { board, budgets = h.BoardReads, h.BudgetReads })
		return
	}
	if bo, _ := reads(); bo != 1 {
		t.Fatalf("board reads %d, want one for both quota holds", bo)
	}
	// Nothing is due until the check times the fleet set: a round reads nothing.
	if n, err := g.Due(ctx); err != nil || n != 0 {
		t.Fatalf("idle round: %d %v", n, err)
	}
	if bo, _ := reads(); bo != 1 {
		t.Fatal("an idle round read the board")
	}

	// quota.window.recovered: the board is read again at once, past its 60 s cache, and the hold recovers.
	f.Set(func(h *hpfake.HP) { h.Board = boardJSON(t, board(up("a"), up("local"))) })
	g.Kick()
	if woke != 1 {
		t.Fatal("a kick doesn't wake the deadline timer")
	}
	if at, _ := g.Next(ctx); !at.Equal(now) {
		t.Fatalf("a kicked quota hold isn't due: %v", at)
	}
	if n, err := g.Due(ctx); err != nil || n != 2 {
		t.Fatalf("kicked round: %d %v", n, err)
	}
	if ids, ap := holds.take(); !slices.Equal(ids, []string{"ulm_q1", "ulm_q2"}) || !ap[0].Recovered || !ap[1].Recovered {
		t.Fatalf("kicked: %v %+v", ids, ap)
	}
	if bo, _ := reads(); bo != 2 {
		t.Fatalf("board reads %d after the kick", bo)
	}

	// A failing board holds the hold back for 2 minutes (not every tick), and a later round reads again.
	holds.mu.Lock()
	holds.holds = append(holds.holds, fleet.GatewayHold{ID: "ulm_q3", SpaceID: "spc_a", Ref: "harbor-pilot", Kind: "gateway_quota", Source: "transcript"})
	holds.mu.Unlock()
	f.Set(func(h *hpfake.HP) { h.FailBoard = 503 })
	advance(2 * time.Minute) // past the 60 s cache
	if n, err := g.Due(ctx); err != nil || n != 0 {
		t.Fatalf("failing board: %d %v", n, err)
	}
	heldBack := func(id string) time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.retry[id].at }
	if at := heldBack("ulm_q3"); !at.Equal(now.Add(gatewayRetry)) {
		t.Fatalf("held back to %v, want %v", at, now.Add(gatewayRetry))
	}
	// Exhausted with no reset named: no estimate, the probe stays in charge, read again in 15 minutes.
	f.Set(func(h *hpfake.HP) { h.FailBoard = 0; h.Board = boardJSON(t, board(down("a", nil))) })
	advance(gatewayRetry)
	if n, err := g.Due(ctx); err != nil || n != 0 {
		t.Fatalf("no reset: %d %v", n, err)
	}
	if at := heldBack("ulm_q3"); !at.Equal(now.Add(fleet.GatewayRecheck)) {
		t.Fatalf("no reset: held back to %v", at)
	}
	if ids, _ := holds.take(); len(ids) != 0 {
		t.Fatalf("applied without an estimate: %v", ids)
	}
}

// Without a service-principal token nothing can be estimated: the holds keep their probe and are looked at again in 15
// minutes, not on every tick.
func TestGatewayWaitsWithoutServiceToken(t *testing.T) {
	t.Parallel()
	svc, _ := newSvc(t)
	b := newBudgets(svc)
	now := mustTime(t, "2026-11-14T10:00:00Z")
	b.now = func() time.Time { return now }
	holds := &fakeHolds{holds: []fleet.GatewayHold{{ID: "ulm_q1", Ref: "harbor-pilot", Kind: "gateway_quota", Source: "transcript"}}}
	g := NewGatewayWaits(b, holds, nil)
	if n, err := g.Due(context.Background()); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if at, _ := g.Next(context.Background()); !at.Equal(now.Add(fleet.GatewayRecheck)) {
		t.Fatalf("next %v", at)
	}
}

// gatewayWaitsOn is a gateway re-check over hpfake with one quota hold and a fake clock.
func gatewayWaitsOn(t *testing.T, holds ...fleet.GatewayHold) (*GatewayWaits, *fakeHolds, *hpfake.HP, func() time.Time, func(time.Duration)) {
	t.Helper()
	svc, _ := newSvc(t)
	f := hpfake.New(t)
	gatewaySpace(t, svc, f, SpaceInput{})
	b := newBudgets(svc)
	now := mustTime(t, "2026-11-14T10:00:00Z")
	var clk sync.Mutex
	b.now = func() time.Time { clk.Lock(); defer clk.Unlock(); return now }
	advance := func(d time.Duration) { clk.Lock(); now = now.Add(d); clk.Unlock() }
	fh := &fakeHolds{holds: holds, now: b.now}
	return NewGatewayWaits(b, fh, nil), fh, f, b.now, advance
}

func quotaHold(id string) fleet.GatewayHold {
	return fleet.GatewayHold{ID: id, SpaceID: "spc_a", Ref: "harbor-pilot", Kind: "gateway_quota", Source: "transcript", Version: 1}
}

// ADR-0079 decision 9: a quota hold follows the accounts behind its estimate. One of them moving its until re-times
// the hold (it never clears it), an unchanged board writes nothing and is read again in 15 minutes, a kicked round
// leaves the hold due at its own time again, and the hold clears only when one of those accounts is available.
func TestGatewayWaitsQuotaFollowsItsAccounts(t *testing.T) {
	t.Parallel()
	g, holds, f, now, advance := gatewayWaitsOn(t, quotaHold("ulm_q"))
	ctx := context.Background()
	at := func(d time.Duration) *time.Time { v := now().Add(d); return &v }
	t0 := now()
	setBoard := func(b *QuotaBoard) { f.Set(func(h *hpfake.HP) { h.Board = boardJSON(t, b) }) }
	round := func(want int) {
		t.Helper()
		if n, err := g.Due(ctx); err != nil || n != want {
			t.Fatalf("round wrote %d (%v), want %d", n, err, want)
		}
	}
	current := func() fleet.GatewayHold {
		holds.mu.Lock()
		defer holds.mu.Unlock()
		if len(holds.holds) == 0 {
			return fleet.GatewayHold{}
		}
		return holds.holds[0]
	}

	b3h, b1h := at(3*time.Hour), at(time.Hour)
	setBoard(board(down("a", b3h), down("b", b1h), up("local")))
	round(1)
	if h := current(); !h.ResetsAt.Equal(*b1h) {
		t.Fatalf("first estimate %+v", h)
	}

	// b is still out, its until moved to 2 h: the hold is re-timed, not cleared.
	advance(fleet.GatewayRecheck)
	b2h := t0.Add(2 * time.Hour)
	setBoard(board(down("a", b3h), down("b", &b2h), up("local")))
	round(1)
	if h := current(); h.ID == "" || !h.ResetsAt.Equal(b2h) {
		t.Fatalf("moved until: %+v", h)
	}
	if _, ap := holds.take(); len(ap) != 2 || ap[1].Recovered {
		t.Fatalf("applied %+v", ap)
	}

	// The same board again: nothing written, the next read in 15 minutes all the same.
	advance(fleet.GatewayRecheck)
	writes := holds.writes
	round(0)
	if holds.writes != writes {
		t.Fatal("an unchanged estimate wrote")
	}
	if next, _ := g.Next(ctx); !next.Equal(now().Add(fleet.GatewayRecheck)) {
		t.Fatalf("next read after an unchanged estimate: %v", next.Sub(now()))
	}

	// A kick makes the hold due now; once the kicked round read the board, the hold is due at its own time again.
	g.Kick()
	if next, _ := g.Next(ctx); !next.Equal(now()) {
		t.Fatalf("kicked: next in %v", next.Sub(now()))
	}
	round(0)
	if next, _ := g.Next(ctx); !next.After(now()) {
		t.Fatalf("still due right after the kicked round: %v", next.Sub(now()))
	}
	if n, _ := g.Due(ctx); n != 0 {
		t.Fatal("a round right after the kicked one wrote")
	}

	// Another account coming back doesn't clear it; b coming back does.
	advance(fleet.GatewayRecheck)
	setBoard(board(up("a"), down("b", &b2h), up("local")))
	round(0)
	if current().ID == "" {
		t.Fatal("another account's recovery cleared the hold")
	}
	advance(fleet.GatewayRecheck)
	setBoard(board(down("a", b3h), up("b"), up("local")))
	round(1)
	if current().ID != "" {
		t.Fatal("the expected account's recovery didn't clear the hold")
	}
}

// A board that lists no account (empty, or account_health missing or renamed) and a blocking budget whose period end
// has passed are no estimate: nothing is written, the hold keeps its probe, and it is read again later (15 minutes for
// the board, 2 minutes for the budget, whose new period should start any moment).
func TestGatewayWaitsNoEstimate(t *testing.T) {
	t.Parallel()
	budget := fleet.GatewayHold{ID: "ulm_b", SpaceID: "spc_a", Ref: "harbor-pilot:project:prj_web", Kind: "gateway_budget", ProjectID: "prj_web",
		Source: "transcript", Version: 1}
	g, holds, f, now, _ := gatewayWaitsOn(t, quotaHold("ulm_q"), budget)
	f.Set(func(h *hpfake.HP) {
		h.Board = `{"accounts":[]}`
		h.Budgets = items(budgetEnding(budgetJSON("bdg_1", "daily", ScopeProject, "prj_web", EnforceBlock, BudgetExceeded, true, 1e9, 12e8), now().Add(-time.Minute)))
	})
	if n, err := g.Due(context.Background()); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if ids, _ := holds.take(); len(ids) != 0 {
		t.Fatalf("applied %v", ids)
	}
	g.mu.Lock()
	q, b := g.retry["ulm_q"].at, g.retry["ulm_b"].at
	g.mu.Unlock()
	if !q.Equal(now().Add(fleet.GatewayRecheck)) || !b.Equal(now().Add(gatewayRetry)) {
		t.Fatalf("held back: quota %v, budget %v", q.Sub(now()), b.Sub(now()))
	}
}

// A Kick while a board read is in flight isn't lost: the read that started before it is neither cached nor used, the
// board is read again in the same round, and the hold recovers. A quota hold that wasn't due when the round started
// stays kicked, so the next round reads it at once.
func TestGatewayWaitsKickDuringRead(t *testing.T) {
	t.Parallel()
	g, holds, f, now, _ := gatewayWaitsOn(t, quotaHold("ulm_q"))
	reset, check := now().Add(time.Hour), now().Add(fleet.GatewayRecheck)
	waiting := quotaHold("ulm_q2")
	waiting.Source, waiting.ResetsAt, waiting.CheckAt = fleet.SourceQuotaBoard, &reset, &check
	holds.holds = append(holds.holds, waiting)
	var once sync.Once
	f.Set(func(h *hpfake.HP) {
		h.Board = boardJSON(t, board(down("a", &reset)))
		h.Hook = func(r *http.Request) {
			if r.URL.Path != "/api/v1/quota/board" {
				return
			}
			once.Do(func() {
				g.Kick()
				f.Set(func(h *hpfake.HP) { h.Board = boardJSON(t, board(up("a"))) })
			})
		}
	})
	ctx := context.Background()
	if n, err := g.Due(ctx); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if ids, ap := holds.take(); len(ap) != 1 || !ap[0].Recovered || ids[0] != "ulm_q" {
		t.Fatalf("applied %v %+v", ids, ap)
	}
	var reads int
	f.Set(func(h *hpfake.HP) { reads = h.BoardReads })
	if reads != 2 {
		t.Fatalf("board reads %d", reads)
	}
	if next, _ := g.Next(ctx); !next.Equal(now()) {
		t.Fatalf("the kick was lost: next in %v", next.Sub(now()))
	}
	if n, err := g.Due(ctx); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if ids, ap := holds.take(); len(ap) != 1 || !ap[0].Recovered || ids[0] != "ulm_q2" {
		t.Fatalf("applied %v %+v", ids, ap)
	}
}
