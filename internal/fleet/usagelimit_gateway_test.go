package fleet

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

// gatewayLimited seeds a live worker of space A routed through Harbor Pilot (project prj_web) at a gateway limit with
// no reset, seen at t0, after one finished turn.
func (e *parkEnv) gatewayLimited(id, kind string) {
	e.t.Helper()
	e.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, session_id, turn, desired, observed, launch_json,
		created_at, created_by, updated_at, observed_at, limit_kind, limit_since)
		VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', ?, ?, 1, 'running', 'running', ?, 0, 't', 0, 0, ?, ?)`,
		id, e.instance(id), "ses_"+id, `{"gateway":{"kind":"harbor-pilot","project_id":"prj_web"},"prompt":"do the task"}`, kind, e.us(0))
}

func (e *parkEnv) gatewayHold(id string) GatewayHold {
	e.t.Helper()
	hs, err := e.svc.OpenGatewayHolds(e.ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, h := range hs {
		if h.ID == id {
			return h
		}
	}
	e.t.Fatalf("gateway hold %s is not open: %+v", id, hs)
	return GatewayHold{}
}

func (e *parkEnv) limitRow(id string) db.UsageLimit {
	e.t.Helper()
	u, err := e.s.R().GetUsageLimit(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return u
}

// SM-RUN-005 (simulated): a parked gateway run's wait under a blocking-budget hold resumes at the budget's period end
// (plus the resume slack) once budget status estimated it; the estimated hold refuses launches until then with the
// reset in its text, its check_at is a re-read and never a probe, and a recovery clears it and wakes the wait.
func TestGatewayBudgetWaitUsesPeriodEnd(t *testing.T) {
	e := newParkEnv(t)
	ref := gatewayRefHPProject + "prj_web"
	e.hold("ulm_b", "spc_a", "", ref, proto.LimitGatewayBudget, nil, dur(limitProbeFirst))
	e.gatewayLimited("run_g", proto.LimitGatewayBudget)
	e.end("run_g", proto.RunExited, "exited")
	w := e.waitOf("run_g")
	if w.UsageLimitID != "ulm_b" || w.ResumeAt != e.us(limitProbeFirst) {
		t.Fatalf("parked before the estimate: %+v", w)
	}
	h := e.gatewayHold("ulm_b")
	if h.Quota() || h.ProjectID != "prj_web" || h.Estimated() {
		t.Fatalf("hold %+v", h)
	}

	e.clk.Advance(time.Minute)
	periodEnd := e.t0.Add(5 * time.Hour)
	wrote, err := e.svc.ApplyGatewayEstimate(e.ctx, h, GatewayEstimate{Source: SourceBudgetStatus, ResetsAt: periodEnd})
	if err != nil || !wrote {
		t.Fatalf("estimate: %v %v", wrote, err)
	}
	u := e.limitRow("ulm_b")
	if u.Source != SourceBudgetStatus || u.ResetsAt.Int64 != periodEnd.UnixMicro() || u.CheckAt.Int64 != e.clk.Now().Add(GatewayRecheck).UnixMicro() {
		t.Fatalf("estimated hold: %+v", u)
	}
	if w := e.waitOf("run_g"); w.ResumeAt != periodEnd.Add(limitResumeSlack).UnixMicro() || w.State != WaitWaiting {
		t.Fatalf("the wait isn't re-timed to the period end: resume at %v", time.UnixMicro(w.ResumeAt))
	}
	target := HoldTarget{GatewayRefs: []string{gatewayRefHP, ref}}
	he, err := e.svc.HoldOpen(e.ctx, target)
	if err != nil || he == nil || !he.Known || !he.Until.Equal(periodEnd) {
		t.Fatalf("hold open: %+v %v", he, err)
	}
	if msg := he.Error(); !strings.Contains(msg, "until 2026-11-08 22:33 UTC, when the budget's period ends") {
		t.Fatalf("refusal text %q", msg)
	}
	// Its check_at is due: still no probe, the hold keeps refusing until the reset.
	e.clk.Advance(GatewayRecheck)
	blocking, probes := holdState([]db.UsageLimit{e.limitRow("ulm_b")}, e.clk.Now().UnixMicro())
	if len(blocking) != 1 || len(probes) != 0 {
		t.Fatalf("an estimated hold became a probe: %d blocking, %d probes", len(blocking), len(probes))
	}

	// The same estimate again moves only the re-check; a read of an older version writes nothing.
	before, wv := e.writeCount(), e.waitOf("run_g").Version
	h = e.gatewayHold("ulm_b")
	if wrote, err := e.svc.ApplyGatewayEstimate(e.ctx, h, GatewayEstimate{Source: SourceBudgetStatus, ResetsAt: periodEnd}); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if w := e.waitOf("run_g"); w.ResumeAt != periodEnd.Add(limitResumeSlack).UnixMicro() || w.Version != wv {
		t.Fatal("an unchanged estimate re-timed the wait")
	}
	if e.limitRow("ulm_b").CheckAt.Int64 != e.clk.Now().Add(GatewayRecheck).UnixMicro() {
		t.Fatal("the re-check didn't move on")
	}
	mid := e.writeCount()
	if wrote, err := e.svc.ApplyGatewayEstimate(e.ctx, h, GatewayEstimate{Source: SourceBudgetStatus, Recovered: true}); err != nil || wrote {
		t.Fatalf("a stale read wrote: %v %v", wrote, err)
	}
	if e.writeCount() != mid || mid == before {
		t.Fatalf("writes %d → %d → %d", before, mid, e.writeCount())
	}

	// The budget no longer blocks: the hold clears (cleared_by budget_status) and its wait is due now.
	if wrote, err := e.svc.ApplyGatewayEstimate(e.ctx, e.gatewayHold("ulm_b"), GatewayEstimate{Source: SourceBudgetStatus, Recovered: true}); err != nil || !wrote {
		t.Fatal(wrote, err)
	}
	if u := e.limitRow("ulm_b"); !u.ClearedAt.Valid || u.ClearedBy != SourceBudgetStatus {
		t.Fatalf("not cleared: %+v", u)
	}
	if w := e.waitOf("run_g"); w.ResumeAt != e.clk.Now().UnixMicro() {
		t.Fatalf("the wait wasn't woken: %v", time.UnixMicro(w.ResumeAt).Sub(e.clk.Now()))
	}
	if he, err := e.svc.HoldOpen(e.ctx, target); err != nil || he != nil {
		t.Fatalf("still held: %+v %v", he, err)
	}
}

// The quota hold: a board estimate earlier than the probe brings the parked wait forward, a later one puts it off,
// and a wait put off as account_held on a direct account is left alone. A run parked after the estimate waits for it.
func TestGatewayQuotaEstimateRetimesWaits(t *testing.T) {
	e := newParkEnv(t)
	e.hold("ulm_q", "spc_a", "", gatewayRefHP, proto.LimitGatewayQuota, nil, dur(limitProbeFirst))
	e.gatewayLimited("run_g", proto.LimitGatewayQuota)
	e.end("run_g", proto.RunExited, "exited")
	e.limited("run_d", "worker", proto.LimitFiveHour, dur(time.Hour), 0, 1)
	e.end("run_d", proto.RunExited, "exited")
	e.exec(`UPDATE limit_waits SET last_error = 'account_held', resume_at = ? WHERE run_id = 'run_d'`, e.us(3*time.Hour))

	soon := e.t0.Add(20 * time.Minute)
	if _, err := e.svc.ApplyGatewayEstimate(e.ctx, e.gatewayHold("ulm_q"), GatewayEstimate{Source: SourceQuotaBoard, ResetsAt: soon}); err != nil {
		t.Fatal(err)
	}
	if w := e.waitOf("run_g"); w.ResumeAt != soon.Add(limitResumeSlack).UnixMicro() {
		t.Fatalf("not brought forward: %v", time.UnixMicro(w.ResumeAt))
	}
	if w := e.waitOf("run_d"); w.ResumeAt != e.us(3*time.Hour) {
		t.Fatal("a direct account's wait was re-timed by the gateway estimate")
	}
	he, _ := e.svc.HoldOpen(e.ctx, HoldTarget{GatewayRefs: []string{gatewayRefHP}})
	if he == nil || !strings.Contains(he.Error(), "until about 2026-11-08 17:53 UTC (estimated from Harbor Pilot's quota board") {
		t.Fatalf("refusal %v", he)
	}
	later := e.t0.Add(4 * time.Hour)
	if _, err := e.svc.ApplyGatewayEstimate(e.ctx, e.gatewayHold("ulm_q"), GatewayEstimate{Source: SourceQuotaBoard, ResetsAt: later}); err != nil {
		t.Fatal(err)
	}
	if w := e.waitOf("run_g"); w.ResumeAt != later.Add(limitResumeSlack).UnixMicro() {
		t.Fatalf("not put off: %v", time.UnixMicro(w.ResumeAt))
	}
	e.gatewayLimited("run_g2", proto.LimitGatewayQuota)
	e.end("run_g2", proto.RunExited, "exited")
	if w := e.waitOf("run_g2"); w.UsageLimitID != "ulm_q" || w.ResumeAt != later.Add(limitResumeSlack).UnixMicro() {
		t.Fatalf("parked after the estimate: %+v", w)
	}
	if _, err := e.svc.ApplyGatewayEstimate(e.ctx, e.gatewayHold("ulm_q"), GatewayEstimate{Source: SourceQuotaBoard}); err == nil {
		t.Fatal("an estimate without a reset was accepted")
	}
}
