package fleet

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/accounts"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

var holdActor = audit.Actor{Type: audit.ActorUser, ID: "usr_op"}

// holdEnv is ulEnv with leases on its three spaces, the real account resolution and a settable failover switch and
// keep-alive cap. Space A binds acc_1 (default) and acc_2; B binds acc_1; C binds acc_2.
type holdEnv struct {
	*staleEnv
	failover bool
	keepMax  time.Duration
	cleared  [][]ClearedHold
}

func newHoldEnv(t *testing.T) *holdEnv {
	t.Helper()
	e := &holdEnv{staleEnv: ulEnv(t), keepMax: 6 * time.Hour}
	e.exec(`UPDATE space_accounts SET is_default = 1 WHERE space_id = 'spc_a' AND account_id = 'acc_1'`)
	e.exec(`INSERT INTO space_accounts (space_id, account_id, harness, created_at, created_by) VALUES ('spc_a', 'acc_2', 'claude-code', 1, 't')`)
	e.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES
		('spc_a', 'nod_1', 1, 'active', 0, 0, ?), ('spc_b', 'nod_1', 1, 'active', 0, 0, ?), ('spc_c', 'nod_1', 1, 'active', 0, 0, ?)`,
		e.us(1000*time.Hour), e.us(1000*time.Hour), e.us(1000*time.Hour))
	e.svc.opts.ResolveAccount = accounts.Resolve
	e.svc.opts.UsageLimitSettings = func(context.Context, *db.Queries, string) UsageLimitSettings {
		return UsageLimitSettings{Failover: e.failover, KeepAliveMax: e.keepMax}
	}
	var mu sync.Mutex
	e.svc.opts.OnHoldsCleared = func(hs []ClearedHold) { mu.Lock(); e.cleared = append(e.cleared, hs); mu.Unlock() }
	return e
}

// hold seeds an open hold. resets and check are offsets from t0; nil is NULL.
func (e *holdEnv) hold(id, space, account, gatewayRef, kind string, resets, check *time.Duration) {
	e.t.Helper()
	scope := LimitScopeAccount
	if gatewayRef != "" {
		scope = LimitScopeGateway
	}
	at := func(d *time.Duration) any {
		if d == nil {
			return nil
		}
		return e.us(*d)
	}
	e.exec(`INSERT INTO usage_limits (id, space_id, scope, account_id, gateway_ref, kind, resets_at, check_at, source, first_run_id, seen_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'transcript', 'run_x', ?, ?, ?)`, id, space, scope, account, gatewayRef, kind, at(resets), at(check), e.us(0), e.us(0), e.us(0))
}

func dur(d time.Duration) *time.Duration { return &d }

func (e *holdEnv) launch(space, account string) (*Run, error) {
	return e.svc.LaunchRun(e.ctx, holdActor, space, LaunchInput{Kind: "worker", Role: "implementor", Harness: "claude-code", Prompt: "go", AccountRef: account})
}

// writes counts the rows a launch could write: runs, commands, change records, events and audit events.
func (e *holdEnv) writes() int {
	return e.count(`SELECT (SELECT count(*) FROM runs) + (SELECT count(*) FROM node_commands) + (SELECT count(*) FROM changes) +
		(SELECT count(*) FROM event_outbox) + (SELECT count(*) FROM audit_events)`)
}

func (e *holdEnv) checkAt(id string) *time.Time {
	e.t.Helper()
	u, err := e.s.R().GetUsageLimit(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return microsPtr(u.CheckAt)
}

func manifestOf(t *testing.T, r *Run) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// wantHold returns the refusal's hold (a copy: not an error, so a caller may ignore it).
func wantHold(t *testing.T, err error, parts ...string) HoldError {
	t.Helper()
	h, ok := AsHold(err)
	if !ok || apperr.CodeOf(err) != apperr.CodeRateLimited {
		t.Fatalf("want a usage-limit refusal, got %v", err)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Fatalf("refusal %q lacks %q", err.Error(), p)
		}
	}
	return *h
}

// ADR-0079 decision 7 (UL-6): a launch whose account is held in any space is refused in the launch transaction with
// the reset time in UTC, and the refusal writes nothing (no run, command, change record, event or audit event). Every
// kind holds the whole account, the model-specific weekly ones included. Another account isn't held, and a hold whose
// reset passed no longer refuses.
func TestUsageLimitLaunchRefused(t *testing.T) {
	e := newHoldEnv(t)
	e.hold("ulm_1", "spc_a", "acc_1", "", proto.LimitFiveHour, dur(3*time.Hour), nil)
	before := e.writes()
	_, err := e.launch("spc_b", "")
	h := wantHold(t, err, `account "one"`, "5-hour", "until 2026-11-08 20:33 UTC")
	if !h.Known || !h.Until.Equal(e.t0.Add(3*time.Hour)) || h.Target.AccountID != "acc_1" {
		t.Fatalf("hold error %+v", h)
	}
	if strings.Contains(err.Error(), "spc_a") || strings.Contains(err.Error(), "ulm_1") {
		t.Fatalf("the refusal names the other space's hold: %v", err)
	}
	if n := e.writes(); n != before {
		t.Fatalf("a refused launch wrote %d rows", n-before)
	}
	// The space that opened the hold is refused too, and a named account is no way around it.
	_, err = e.launch("spc_a", "acc_1")
	wantHold(t, err, "until 2026-11-08 20:33 UTC")
	// Another account is not held.
	if _, err := e.launch("spc_c", ""); err != nil {
		t.Fatalf("space C (another account): %v", err)
	}

	// The weekly Opus limit holds the account for every model; the latest reset decides the time.
	e.hold("ulm_2", "spc_b", "acc_1", "", proto.LimitSevenDayOpus, dur(50*time.Hour), nil)
	_, err = e.launch("spc_b", "")
	wantHold(t, err, "weekly Opus", "until 2026-11-10 19:33 UTC")

	// Past both resets, the holds (not yet cleared by the deadline job) no longer refuse.
	e.clk.Advance(50 * time.Hour)
	if _, err := e.launch("spc_b", ""); err != nil {
		t.Fatalf("after the reset: %v", err)
	}
}

// ADR-0079 decision 7 (UL-6): a hold whose reset is unknown refuses with its probe time; when the probe is due exactly
// one launch gets through (it moves check_at on in its own transaction), and the next probe backs off from 1 h,
// doubling, capped at the keep-alive maximum. A hold whose reset is known and that another space's run asked to probe
// (check_at = now) lets one launch through, then waits for its reset.
func TestUsageLimitProbe(t *testing.T) {
	e := newHoldEnv(t)
	e.keepMax = 3 * time.Hour
	e.hold("ulm_u", "spc_a", "acc_1", "", proto.LimitUnknown, nil, dur(time.Hour))
	_, err := e.launch("spc_b", "")
	if h := wantHold(t, err, "reset time unknown, re-checking at 2026-11-08 18:33 UTC"); h.Known {
		t.Fatalf("an unknown reset reported as known: %+v", h)
	}
	// Each probe time lets exactly one launch through; the gaps are 1 h, 2 h, then the 3 h cap.
	for i, gap := range []time.Duration{time.Hour, 2 * time.Hour, 3 * time.Hour, 3 * time.Hour} {
		e.clk.Set(*e.checkAt("ulm_u"))
		probeAt := e.clk.Now()
		r, err := e.launch("spc_b", "")
		if err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
		if manifestOf(t, r)["usage_limit_probe"] != true {
			t.Fatalf("probe %d: manifest %s", i, r.Manifest)
		}
		if next := e.checkAt("ulm_u"); next == nil || !next.Equal(probeAt.Add(gap)) {
			t.Fatalf("probe %d: next check %v, want %v", i, next, probeAt.Add(gap))
		}
		before := e.writes()
		for range 3 {
			if _, err := e.launch("spc_a", ""); err == nil {
				t.Fatalf("probe %d: a second launch got through", i)
			}
		}
		if e.writes() != before {
			t.Fatalf("probe %d: refusals wrote", i)
		}
	}

	// A known reset asked to be probed now: one launch, then refused until the reset.
	e2 := newHoldEnv(t)
	e2.hold("ulm_k", "spc_b", "acc_1", "", proto.LimitSevenDay, dur(30*time.Hour), dur(0))
	if _, err := e2.launch("spc_a", ""); err != nil {
		t.Fatalf("evidence probe: %v", err)
	}
	if c := e2.checkAt("ulm_k"); c != nil {
		t.Fatalf("a known reset's probe left check_at %v", c)
	}
	_, err = e2.launch("spc_a", "")
	wantHold(t, err, "weekly", "until 2026-11-09 23:33 UTC")
}

// ADR-0079 decision 8, Q1 (UL-6): failover is off by default. With it on, a held account gives way to another bound
// account of the harness without a hold (recorded in the manifest); with every account held the launch is refused. A
// space routed through the gateway never falls back to a direct account.
func TestUsageLimitFailover(t *testing.T) {
	e := newHoldEnv(t)
	e.hold("ulm_1", "spc_b", "acc_1", "", proto.LimitFiveHour, dur(time.Hour), nil)
	_, err := e.launch("spc_a", "")
	wantHold(t, err, `account "one"`)

	e.failover = true
	r, err := e.launch("spc_a", "")
	if err != nil {
		t.Fatalf("failover: %v", err)
	}
	if r.Launch.AccountRef != "acc_2" {
		t.Fatalf("failover used %q", r.Launch.AccountRef)
	}
	if fo, _ := manifestOf(t, r)["account_failover"].(map[string]any); fo["from"] != "acc_1" || fo["to"] != "acc_2" {
		t.Fatalf("manifest %s", r.Manifest)
	}
	// A resume names its old account: it fails over too.
	if r, err := e.launch("spc_a", "acc_1"); err != nil || r.Launch.AccountRef != "acc_2" {
		t.Fatalf("named: %v %+v", err, r)
	}
	// A space with no other account is still refused.
	_, err = e.launch("spc_b", "")
	wantHold(t, err, `account "one"`)
	// Every account held: refused, naming the usual choice.
	e.hold("ulm_2", "spc_c", "acc_2", "", proto.LimitSevenDay, dur(2*time.Hour), nil)
	_, err = e.launch("spc_a", "")
	wantHold(t, err, `account "one"`)

	// Gateway space C, failover on, a direct account bound and not held: refused all the same.
	g := newHoldEnv(t)
	g.failover = true
	g.svc.opts.Gateway = routeGateway{space: "spc_c", project: "prj_1"}
	g.hold("ulm_g", "spc_c", "", gatewayRefHP, proto.LimitGatewayQuota, nil, dur(time.Hour))
	_, err = g.launch("spc_c", "")
	wantHold(t, err, "Harbor Pilot gateway", "re-checking at 2026-11-08 18:33 UTC")
	if n := g.count(`SELECT count(*) FROM runs`); n != 0 {
		t.Fatalf("%d runs", n)
	}
	// A project's budget hold holds that project's spaces.
	g2 := newHoldEnv(t)
	g2.svc.opts.Gateway = routeGateway{space: "spc_c", project: "prj_1"}
	g2.hold("ulm_p", "spc_c", "", gatewayRefHPProject+"prj_1", proto.LimitGatewayBudget, dur(5*time.Hour), nil)
	_, err = g2.launch("spc_c", "")
	wantHold(t, err, "project budget", "until 2026-11-08 22:33 UTC")
	g2.svc.opts.Gateway = routeGateway{space: "spc_c", project: "prj_2"}
	if _, err := g2.launch("spc_c", ""); err != nil {
		t.Fatalf("another project: %v", err)
	}
}

// ADR-0079 decision 7 (UL-6): an operator's space-level "clear now" clears only that space's row and says the account
// is still held elsewhere; an account-level clear clears every space's rows (of one kind, or all). Each is audited
// once, publishes one cleared event per row, and reports the cleared holds for their spaces' control. Unknown holds
// and other spaces' holds are not found.
func TestUsageLimitClearNow(t *testing.T) {
	e := newHoldEnv(t)
	e.hold("ulm_a5", "spc_a", "acc_1", "", proto.LimitFiveHour, dur(3*time.Hour), nil)
	e.hold("ulm_b5", "spc_b", "acc_1", "", proto.LimitFiveHour, dur(3*time.Hour), nil)
	e.hold("ulm_a7", "spc_a", "acc_1", "", proto.LimitSevenDay, dur(30*time.Hour), nil)

	if _, err := e.svc.ClearSpaceUsageLimit(e.ctx, holdActor, "spc_b", "ulm_a5"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("another space's hold: %v", err)
	}
	res, err := e.svc.ClearSpaceUsageLimit(e.ctx, holdActor, "spc_a", "ulm_a5")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Cleared) != 1 || res.Cleared[0].ID != "ulm_a5" || res.Cleared[0].ClearedBy != "user:usr_op" || !res.HeldElsewhere || res.Note == "" {
		t.Fatalf("space clear %+v", res)
	}
	if n := e.count(`SELECT count(*) FROM usage_limits WHERE cleared_at IS NULL`); n != 2 {
		t.Fatalf("%d open holds after a space clear", n)
	}
	if _, err := e.svc.ClearSpaceUsageLimit(e.ctx, holdActor, "spc_a", "ulm_a5"); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("clearing twice: %v", err)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = 'usage_limit.cleared' AND target_id = 'ulm_a5' AND actor_id = 'usr_op'`); n != 1 {
		t.Fatalf("%d space-clear audit events", n)
	}
	if len(e.cleared) != 1 || len(e.cleared[0]) != 1 || e.cleared[0][0].SpaceID != "spc_a" || e.cleared[0][0].AccountName != "one" {
		t.Fatalf("told %+v", e.cleared)
	}
	// Space B is still refused: its own row holds the account.
	_, err = e.launch("spc_b", "")
	wantHold(t, err, "until")

	if _, err := e.svc.ClearAccountUsageLimits(e.ctx, holdActor, "acc_1", "gateway_quota"); apperr.CodeOf(err) != apperr.CodeInvalid {
		t.Fatalf("gateway kind: %v", err)
	}
	if _, err := e.svc.ClearAccountUsageLimits(e.ctx, holdActor, "acc_nope", ""); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("unknown account: %v", err)
	}
	res, err = e.svc.ClearAccountUsageLimits(e.ctx, holdActor, "acc_1", proto.LimitFiveHour)
	if err != nil || len(res.Cleared) != 1 || res.Cleared[0].ID != "ulm_b5" {
		t.Fatalf("account clear (five_hour): %v %+v", err, res)
	}
	res, err = e.svc.ClearAccountUsageLimits(e.ctx, holdActor, "acc_1", "")
	if err != nil || len(res.Cleared) != 1 || res.Cleared[0].ID != "ulm_a7" {
		t.Fatalf("account clear (all): %v %+v", err, res)
	}
	// Nothing open: nothing written, nobody told.
	before := e.writes()
	if res, err := e.svc.ClearAccountUsageLimits(e.ctx, holdActor, "acc_1", ""); err != nil || len(res.Cleared) != 0 || e.writes() != before {
		t.Fatalf("empty account clear: %v %+v", err, res)
	}
	if n := e.count(`SELECT count(*) FROM audit_events WHERE action = 'account.usage_limits_cleared' AND target_id = 'acc_1'`); n != 2 {
		t.Fatalf("%d account-clear audit events", n)
	}
	if n := e.count(`SELECT count(*) FROM event_outbox WHERE subject LIKE '%usage_limit.cleared'`); n != 3 {
		t.Fatalf("%d cleared events", n)
	}
	if len(e.cleared) != 3 || e.cleared[1][0].SpaceID != "spc_b" || e.cleared[2][0].SpaceID != "spc_a" {
		t.Fatalf("told %+v", e.cleared)
	}
	if _, err := e.launch("spc_b", ""); err != nil {
		t.Fatalf("after the account clear: %v", err)
	}
}

// routeGateway routes one space's Claude Code runs through Harbor Pilot.
type routeGateway struct{ space, project string }

func (routeGateway) PreLaunch(context.Context, string, string) error { return nil }
func (g routeGateway) Route(_ context.Context, _ *store.Tx, spaceID, _, _, _ string) (*GatewaySpec, error) {
	if spaceID != g.space {
		return nil, nil
	}
	return &GatewaySpec{Kind: GatewayKindHarborPilot, ProjectID: g.project}, nil
}
func (routeGateway) Token(context.Context, *Run, string) (map[string]string, error) { return nil, nil }
func (routeGateway) Address(context.Context) (*proto.GatewayLaunch, error)          { return nil, nil }
