package harborpilot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
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
	recovered, until, known := EstimateQuota(ex, fleet.GatewayHold{Ref: "harbor-pilot", Source: "transcript"}, now)
	if recovered || !known || !until.Equal(mustTime(t, "2026-11-14T05:30:00Z")) {
		t.Fatalf("estimate: %v %v %v", recovered, until, known)
	}

	rec := readBoard(t, "quota_board_v016_recovered.json")
	for _, a := range rec.AccountHealth {
		if !a.Available || a.Until != nil {
			t.Fatalf("recovered board: %+v", a)
		}
	}
	est := fleet.GatewayHold{Ref: "harbor-pilot", Source: fleet.SourceQuotaBoard, ResetsAt: &until}
	if recovered, _, _ := EstimateQuota(rec, est, now); !recovered {
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
	fresh := fleet.GatewayHold{Ref: "harbor-pilot", Source: "transcript"}
	estimatedAt := func(d time.Duration) fleet.GatewayHold {
		return fleet.GatewayHold{Ref: "harbor-pilot", Source: fleet.SourceQuotaBoard, ResetsAt: at(d)}
	}
	for _, c := range []struct {
		name      string
		h         fleet.GatewayHold
		b         *QuotaBoard
		recovered bool
		until     *time.Time
	}{
		{"earliest until wins", fresh, board(down("a", at(3*time.Hour)), down("b", at(time.Hour)), up("local")), false, at(time.Hour)},
		{"past and unknown untils are no estimate", fresh, board(down("a", at(-time.Minute)), down("b", nil)), false, nil},
		{"nothing unavailable recovers", fresh, board(up("a"), up("b")), true, nil},
		{"an empty board recovers", fresh, board(), true, nil},
		{"the expected account is still out", estimatedAt(time.Hour), board(down("a", at(3*time.Hour)), down("b", at(time.Hour))), false, at(time.Hour)},
		{"the expected account came back", estimatedAt(time.Hour), board(down("a", at(3*time.Hour)), up("b")), true, nil},
		{"the expected reset passed", estimatedAt(-time.Minute), board(down("a", at(3*time.Hour))), true, nil},
		{"the expected account moved its until", estimatedAt(time.Hour), board(down("a", at(3*time.Hour)), down("b", at(2*time.Hour))), true, nil},
		{"a newly exhausted account moves the estimate earlier", estimatedAt(time.Hour), board(down("a", at(time.Hour)), down("b", at(10*time.Minute))), false, at(10 * time.Minute)},
		{"sub-second jitter is the same instant", estimatedAt(time.Hour), board(down("a", at(time.Hour+300*time.Millisecond))), false, at(time.Hour + 300*time.Millisecond)},
	} {
		recovered, until, known := EstimateQuota(c.b, c.h, now)
		if recovered != c.recovered || known != (c.until != nil) || (c.until != nil && !until.Equal(*c.until)) {
			t.Errorf("%s: got %v %v %v, want %v %v", c.name, recovered, until, known, c.recovered, c.until)
		}
	}
}

func TestEstimateBudget(t *testing.T) {
	t.Parallel()
	end1, end2 := mustTime(t, "2026-11-15T00:00:00Z"), mustTime(t, "2026-12-01T00:00:00Z")
	blocking := func(end *time.Time) Covering { return Covering{Blocking: true, State: BudgetExceeded, PeriodEnd: end} }
	if r, _, _ := EstimateBudget([]Covering{{State: BudgetWarning}, {State: BudgetExceeded}}); !r {
		t.Fatal("no blocking budget must recover")
	}
	if r, u, k := EstimateBudget([]Covering{blocking(&end2), blocking(&end1), {State: BudgetOK}}); r || !k || !u.Equal(end2) {
		t.Fatalf("latest period end of the blocking budgets: %v %v %v", r, u, k)
	}
	if r, _, k := EstimateBudget([]Covering{blocking(&end1), blocking(nil)}); r || k {
		t.Fatal("a blocking one-time budget without an end has no estimate")
	}
}

// fakeHolds is the fleet side of the gateway re-check.
type fakeHolds struct {
	mu      sync.Mutex
	holds   []fleet.GatewayHold
	applied []fleet.GatewayEstimate
	ids     []string
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
		if f.holds[i].ID != h.ID {
			continue
		}
		if e.Recovered {
			f.holds = slices.Delete(f.holds, i, i+1)
			break
		}
		r, c := e.ResetsAt, f.now().Add(fleet.GatewayRecheck)
		f.holds[i].ResetsAt, f.holds[i].Source, f.holds[i].CheckAt = &r, e.Source, &c
		break
	}
	return true, nil
}

func (f *fakeHolds) take() ([]string, []fleet.GatewayEstimate) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids, ap := f.ids, f.applied
	f.ids, f.applied = nil, nil
	return ids, ap
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
	reset := now.Add(90 * time.Minute)
	f.Set(func(h *hpfake.HP) {
		h.Board = boardJSON(t, board(down("a", &reset), up("local")))
		h.Budgets = items(budgetJSON("bdg_1", "daily", ScopeProject, "prj_web", EnforceBlock, BudgetExceeded, true, 1e9, 12e8))
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
		!ap[2].ResetsAt.Equal(mustTime(t, "2026-11-09T00:00:00Z")) || ap[2].Source != fleet.SourceBudgetStatus {
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
	heldBack := func(id string) time.Time { g.mu.Lock(); defer g.mu.Unlock(); return g.retry[id] }
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
