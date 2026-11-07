package human

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/outbox"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/db"
)

func (f *fx) exec(qs ...string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range qs {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
}

func (f *fx) run(id, kind, role, instance, desired, observed string) {
	f.t.Helper()
	f.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, status, launch_json, created_at, created_by, updated_at)
	        VALUES ('` + id + `', 'spc_a', 'nod_1', 1, '` + kind + `', '` + role + `', 'claude-code', '` + instance + `', '` + desired + `', '` + observed + `', 'idle', '{}', 0, 't', 0)`)
}

func (f *fx) end(run string) {
	f.t.Helper()
	f.exec(`UPDATE runs SET observed = 'exited', desired = 'stopped' WHERE id = '` + run + `'`)
}

func (f *fx) answer(a *Action) *Action {
	f.t.Helper()
	got, err := f.svc.Respond(f.ctx, op, "spc_a", a.ID, 0, RespondInput{Outcome: "answered", Response: Response{Answer: responseMarker}})
	must(f.t, err)
	return got
}

// responseMarker is the test response. It uses only letters Crockford base32 never contains (i, l, o, u), so no random
// ID in a payload can contain it and the "the response is not leaked" checks below cannot match by chance.
const responseMarker = "louilou"

func (f *fx) get(id string) *Action {
	f.t.Helper()
	a, err := f.svc.Get(f.ctx, "spc_a", id)
	must(f.t, err)
	return a
}

func (f *fx) count(q string) int {
	f.t.Helper()
	var n int
	must(f.t, f.s.ReadDB().QueryRowContext(f.ctx, q).Scan(&n))
	return n
}

func (f *fx) policy(space, typ string, p PolicyFields) {
	f.t.Helper()
	_, err := f.svc.SetPolicy(f.ctx, op, space, typ, 0, p)
	must(f.t, err)
}

func question(title string) CreateInput { return CreateInput{Type: TypeQuestion, Title: title} }

// SM-HUM-009: a response for a requester whose run ended goes to a live successor run, else to control, else to the
// operators, always with the original run, the previous target and the reason.
func TestResponsesForAnEndedRequesterAreRerouted(t *testing.T) {
	f := newFx(t)
	f.svc.Outbox = outbox.New(f.s, outbox.Options{Enabled: true, Clock: f.clk})

	// Nobody can take it: undeliverable, visible on the action and as a message to the operators.
	a := f.create(question("Which port?"))
	f.end("run_1")
	n := f.answer(a).Notifications[0]
	if n.TargetKind != TargetOperators || n.OriginalRun != "run_1" || n.RoutedFrom != "run:run_1" || n.RouteReason != ReasonRequesterEnded || n.RoutedAt == nil {
		t.Fatalf("undeliverable %+v", n)
	}
	if f.count(`SELECT count(*) FROM messages WHERE to_kind = 'operators' AND correlation_id = '`+a.ID+`' AND body NOT LIKE '%louilou%'`) != 1 {
		t.Fatal("no operator message (or it carries the response)")
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action = 'human_action.rerouted'`) != 1 ||
		f.count(`SELECT count(*) FROM event_outbox WHERE type = 'spaceman.human.action_rerouted.v1' AND payload_json NOT LIKE '%louilou%'`) != 1 {
		t.Fatal("reroute not audited or published")
	}

	// A wanted control run takes it.
	f.run("run_ctl", "control", "control", "ins_c", "running", "running")
	b := f.create(CreateInput{Type: TypeQuestion, Title: "b", Requester: Requester{Kind: "agent", ID: "ins_2", Role: "worker", Run: "run_gone"}})
	if n := f.answer(b).Notifications[0]; n.TargetKind != TargetControl || n.RouteReason != ReasonRequesterEnded {
		t.Fatalf("to control %+v", n)
	}

	// A live run of the successor role comes first.
	f.policy("spc_a", "all", PolicyFields{MaxReminders: 1, RequesterEnded: EndedReroute, HoldMinutes: 60, SuccessorRole: "reviewer", WakeIdle: true,
		RatePerRunHour: 30, RatePerSpaceHour: 300})
	f.run("run_rev", "worker", "reviewer", "ins_r", "running", "running")
	c := f.create(CreateInput{Type: TypeQuestion, Title: "c", Requester: Requester{Kind: "agent", ID: "ins_3", Role: "worker", Run: "run_gone"}})
	if n := f.answer(c).Notifications[0]; n.TargetKind != TargetRun || n.TargetRun != "run_rev" || n.TargetRole != "reviewer" {
		t.Fatalf("to successor %+v", n)
	}

	// The successor ends before acknowledging: it moves on to control (run_ended), never to an unrelated run.
	f.end("run_rev")
	f.svc.Tick(f.ctx)
	got := f.get(c.ID).Notifications[0]
	if got.TargetKind != TargetControl || got.RouteReason != ReasonRunEnded || got.RoutedFrom != "run:run_rev" || got.OriginalRun != "run_gone" || got.RouteCount != 2 {
		t.Fatalf("after successor ended %+v", got)
	}
}

// SM-HUM-009: a response delivered to a live requester whose run ends before acknowledging it is routed on by the
// deadline loop (also after a restart: the loop's first round does it).
func TestStrandedNotificationsMoveOn(t *testing.T) {
	f := newFx(t)
	f.run("run_ctl", "control", "control", "ins_c", "running", "running")
	a := f.create(question("q"))
	if n := f.answer(a).Notifications[0]; n.TargetKind != TargetRun || n.TargetRun != "run_1" || n.RouteReason != "" {
		t.Fatalf("live requester %+v", n)
	}
	f.svc.Tick(f.ctx)
	if n := f.get(a.ID).Notifications[0]; n.TargetRun != "run_1" {
		t.Fatalf("moved while the requester runs %+v", n)
	}
	f.end("run_1")
	f.svc.Tick(f.ctx)
	if n := f.get(a.ID).Notifications[0]; n.TargetKind != TargetControl || n.RouteReason != ReasonRequesterEnded || n.State != "pending" {
		t.Fatalf("after the requester ended %+v", n)
	}
	// An acknowledged notification stays where it is.
	b := f.create(CreateInput{Type: TypeTodo, Title: "t", Requester: Requester{Kind: "agent", ID: "ins_9", Role: "worker", Run: "run_ctl"}})
	f.answer2(b)
	f.exec(`UPDATE human_notifications SET state = 'acknowledged' WHERE action_id = '` + b.ID + `'`)
	f.end("run_ctl")
	f.svc.Tick(f.ctx)
	if n := f.get(b.ID).Notifications[0]; n.TargetKind != TargetRun || n.State != "acknowledged" {
		t.Fatalf("acknowledged moved %+v", n)
	}
	// Bounded: a notification routed too often goes to the operators.
	f.exec(`UPDATE human_notifications SET route_count = 5, target_kind = 'run', target_run = 'run_ctl' WHERE action_id = '` + a.ID + `'`)
	f.svc.Tick(f.ctx)
	if n := f.get(a.ID).Notifications[0]; n.TargetKind != TargetOperators || n.RouteReason != ReasonTooManyRoutes {
		t.Fatalf("too many routes %+v", n)
	}
}

func (f *fx) answer2(a *Action) {
	f.t.Helper()
	_, err := f.svc.Respond(f.ctx, op, "spc_a", a.ID, 0, RespondInput{Outcome: "completed"})
	must(f.t, err)
}

// SM-HUM-009: with next_launch the response waits for the next run of the requester's agent instance (a resumed
// session) and is rerouted when none starts within the hold.
func TestNextLaunchHoldsForTheInstance(t *testing.T) {
	f := newFx(t)
	f.policy("", TypeQuestion, PolicyFields{MaxReminders: 1, RequesterEnded: EndedNextLaunch, HoldMinutes: 30, WakeIdle: false, RatePerRunHour: 30, RatePerSpaceHour: 300})
	inst := ids.New(ids.Instance)
	f.run("run_k1", "kb", "kb", inst, "running", "running")
	req := Requester{Kind: "agent", ID: inst, Role: "kb", Run: "run_k1"}
	a := f.create(CreateInput{Type: TypeQuestion, Title: "a", Requester: req})
	b := f.create(CreateInput{Type: TypeQuestion, Title: "b", Requester: req})
	f.end("run_k1")
	na := f.answer(a).Notifications[0]
	if na.TargetKind != TargetInstance || na.TargetInstance != inst || na.HoldUntil == nil || !na.HoldUntil.Equal(f.clk.Now().Add(30*time.Minute)) || na.Wake {
		t.Fatalf("held %+v", na)
	}
	if next, err := f.svc.NextDeadline(f.ctx); err != nil || !next.Equal(*na.HoldUntil) {
		t.Fatalf("next deadline %v %v", next, err)
	}
	// The session resumes: the held response is delivered to that run.
	f.run("run_k2", "kb", "kb", inst, "running", "starting")
	f.svc.Tick(f.ctx)
	if n := f.get(a.ID).Notifications[0]; n.TargetKind != TargetRun || n.TargetRun != "run_k2" {
		t.Fatalf("pinned %+v", n)
	}
	// While a run of the instance is live, a new response goes straight to it.
	if n := f.answer(b).Notifications[0]; n.TargetKind != TargetRun || n.TargetRun != "run_k2" || n.RouteReason != ReasonRequesterEnded {
		t.Fatalf("live instance %+v", n)
	}
	// No resume within the hold: rerouted (no control here, so to the operators).
	c := f.create(CreateInput{Type: TypeQuestion, Title: "c", Requester: Requester{Kind: "agent", ID: ids.New(ids.Instance), Role: "kb", Run: "run_k1"}})
	f.answer(c)
	f.clk.Advance(29 * time.Minute)
	f.svc.Tick(f.ctx)
	if n := f.get(c.ID).Notifications[0]; n.TargetKind != TargetInstance {
		t.Fatalf("rerouted before the hold ended %+v", n)
	}
	f.clk.Advance(2 * time.Minute)
	f.svc.Tick(f.ctx)
	if n := f.get(c.ID).Notifications[0]; n.TargetKind != TargetOperators || n.RouteReason != ReasonHoldExpired || !strings.HasPrefix(n.RoutedFrom, "instance:") {
		t.Fatalf("after the hold %+v", n)
	}
}

// A person can send an undeliverable response to control or to a running role.
func TestOperatorRoutesAResponse(t *testing.T) {
	f := newFx(t)
	a := f.create(question("q"))
	f.end("run_1")
	n := f.answer(a).Notifications[0]
	_, err := f.svc.RouteNotification(f.ctx, op, "spc_a", a.ID, n.ID, "role:reviewer")
	code(t, err, apperr.CodePrecondition)
	_, err = f.svc.RouteNotification(f.ctx, op, "spc_a", a.ID, n.ID, "run:run_1")
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.RouteNotification(f.ctx, op, "spc_b", a.ID, n.ID, "control")
	code(t, err, apperr.CodeNotFound)
	f.run("run_rev", "worker", "reviewer", "ins_r", "running", "running")
	got, err := f.svc.RouteNotification(f.ctx, op, "spc_a", a.ID, n.ID, "role:reviewer")
	must(t, err)
	if x := got.Notifications[0]; x.TargetRun != "run_rev" || x.RouteReason != ReasonOperator || x.RoutedFrom != TargetOperators {
		t.Fatalf("routed %+v", x)
	}
	got, err = f.svc.RouteNotification(f.ctx, op, "spc_a", a.ID, n.ID, "control")
	must(t, err)
	if x := got.Notifications[0]; x.TargetKind != TargetControl {
		t.Fatalf("to control %+v", x)
	}
	f.exec(`UPDATE human_notifications SET state = 'acknowledged'`)
	_, err = f.svc.RouteNotification(f.ctx, op, "spc_a", a.ID, n.ID, "control")
	code(t, err, apperr.CodeConflict)
}

// SM-HUM-009: runs unrelated to an action see its request, never its response, comments or reason.
func TestResponsesHiddenFromUnrelatedRuns(t *testing.T) {
	f := newFx(t)
	a := f.create(question("secret?"))
	_, err := f.svc.AddComment(f.ctx, op, "spc_a", a.ID, "between us", "Op")
	must(t, err)
	f.answer(a)
	view := func(v Viewer) *Action {
		l, err := f.svc.ForViewer(f.ctx, "spc_a", v, f.get(a.ID))
		must(t, err)
		return l[0]
	}
	for name, v := range map[string]Viewer{"requester": {RunID: "run_1"}, "same instance": {RunID: "run_x", InstanceID: "ins_1"}, "control": {RunID: "run_c", Control: true}} {
		if got := view(v); got.ResponseHidden || !strings.Contains(string(got.Response), responseMarker) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	other := view(Viewer{RunID: "run_other", InstanceID: "ins_other"})
	if !other.ResponseHidden || string(other.Response) != "{}" || other.Comments != nil || other.Notifications != nil || other.Title != "secret?" {
		t.Fatalf("unrelated: %+v", other)
	}
	// The run a response was routed to may read it.
	f.exec(`UPDATE human_notifications SET target_run = 'run_rev' WHERE action_id = '` + a.ID + `'`)
	if got := view(Viewer{RunID: "run_rev"}); got.ResponseHidden {
		t.Fatalf("routed run: %+v", got)
	}
}

// SM-HUM-011: reminders re-surface an action on schedule without changing its version or order; escalation raises the
// priority (never lowers it) and assigns per policy, audited, and the order stays.
func TestRemindersAndEscalation(t *testing.T) {
	f := newFx(t)
	f.svc.Outbox = outbox.New(f.s, outbox.Options{Enabled: true, Clock: f.clk})
	first := f.create(CreateInput{Type: TypeTodo, Title: "first", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	q := f.create(question("built-in policy"))
	if q.NextReminderAt == nil || !q.NextReminderAt.Equal(f.clk.Now().Add(4*time.Hour)) || q.EscalateAt == nil || !q.EscalateAt.Equal(f.clk.Now().Add(24*time.Hour)) {
		t.Fatalf("schedule %+v", q)
	}
	m := f.create(CreateInput{Type: TypeMessage, Title: "fyi"})
	if m.NextReminderAt != nil || m.EscalateAt != nil {
		t.Fatalf("messages are not reminded by default %+v", m)
	}
	f.clk.Advance(4 * time.Hour)
	if n, err := f.svc.Remind(f.ctx); err != nil || n != 1 {
		t.Fatalf("remind %d %v", n, err)
	}
	r := f.get(q.ID)
	if r.ReminderCount != 1 || r.RemindedAt == nil || r.Version != q.Version || !r.NextReminderAt.Equal(f.clk.Now().Add(24*time.Hour)) {
		t.Fatalf("reminded %+v", r)
	}
	if n, _ := f.svc.Remind(f.ctx); n != 0 {
		t.Fatal("reminded twice")
	}
	f.clk.Advance(20 * time.Hour) // 24h after asking
	f.svc.Tick(f.ctx)
	e := f.get(q.ID)
	if e.Priority != "high" || e.EscalatedFrom != "normal" || e.EscalatedAt == nil || e.EscalateAt != nil || e.Version != q.Version+1 || e.Rank != q.Rank {
		t.Fatalf("escalated %+v", e)
	}
	l, _ := f.svc.List(f.ctx, "spc_a", ListFilter{Pending: true})
	if l[0].ID != first.ID || l[1].ID != q.ID {
		t.Fatal("escalation changed the order")
	}
	// Escalation never lowers a priority, and can assign.
	f.policy("spc_a", TypeTodo, PolicyFields{MaxReminders: 1, EscalateAfterMinutes: 60, EscalatePriority: "high", EscalateAssignee: "usr_op",
		RequesterEnded: EndedReroute, HoldMinutes: 60, WakeIdle: true, RatePerRunHour: 30, RatePerSpaceHour: 300})
	u := f.create(CreateInput{Type: TypeTodo, Title: "urgent", Priority: "urgent"})
	f.clk.Advance(time.Hour)
	if n, err := f.svc.Escalate(f.ctx); err != nil || n != 2 { // "first" (rescheduled by the new policy) and "urgent"
		t.Fatalf("escalate %d %v", n, err)
	}
	if g := f.get(u.ID); g.Priority != "urgent" || g.Assignee != "usr_op" || g.Status != InProgress {
		t.Fatalf("assigned %+v", g)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action = 'human_action.escalated'`) != 3 ||
		f.count(`SELECT count(*) FROM audit_events WHERE action = 'human_action.reminded'`) < 1 ||
		f.count(`SELECT count(*) FROM event_outbox WHERE type = 'spaceman.human.action_reminded.v1'`) < 1 ||
		f.count(`SELECT count(*) FROM event_outbox WHERE type = 'spaceman.human.action_escalated.v1'`) != 3 {
		t.Fatal("reminders/escalation not audited or published")
	}
	// A resolved action is never reminded or escalated again.
	f.answer(f.get(q.ID))
	f.clk.Advance(48 * time.Hour)
	if n, _ := f.svc.Remind(f.ctx); n != 0 {
		t.Fatalf("reminded a resolved action")
	}
}

// Policies: the most specific applies as a whole; changes reschedule pending actions; invalid ones are refused.
func TestPolicyResolution(t *testing.T) {
	f := newFx(t)
	eff := func(space string) map[string]Policy {
		l, err := f.svc.EffectivePolicies(f.ctx, space)
		must(t, err)
		out := map[string]Policy{}
		for _, p := range l {
			out[p.ActionType] = p
		}
		return out
	}
	if e := eff("spc_a"); e[TypeQuestion].Source != "built-in" || e[TypeApproval].RemindAfterMinutes != 60 || len(e) != 5 {
		t.Fatalf("built-in %+v", e)
	}
	base := PolicyFields{RemindAfterMinutes: 10, MaxReminders: 2, RequesterEnded: EndedReroute, HoldMinutes: 60, WakeIdle: true, RatePerRunHour: 30, RatePerSpaceHour: 300}
	a := f.create(question("q"))
	f.policy("", "all", base)
	if e := eff("spc_a"); e[TypeQuestion].Source != "default:all" || e[TypeTodo].RemindAfterMinutes != 10 {
		t.Fatalf("default * %+v", e)
	}
	if g := f.get(a.ID); !g.NextReminderAt.Equal(a.CreatedAt.Add(10*time.Minute)) || g.EscalateAt != nil {
		t.Fatalf("rescheduled %+v", g)
	}
	sp := base
	sp.RemindAfterMinutes = 0
	f.policy("spc_a", TypeQuestion, sp)
	e := eff("spc_a")
	if e[TypeQuestion].Source != "space:question" || e[TypeTodo].Source != "default:all" || eff("spc_b")[TypeQuestion].Source != "default:all" {
		t.Fatalf("space override %+v", e)
	}
	if g := f.get(a.ID); g.NextReminderAt != nil {
		t.Fatalf("reminder kept after the policy dropped it %+v", g)
	}
	l, err := f.svc.Policies(f.ctx, "spc_a")
	must(t, err)
	if len(l) != 1 || l[0].Version != 1 {
		t.Fatalf("space policies %+v", l)
	}
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", TypeQuestion, 7, sp)
	code(t, err, apperr.CodePrecondition)
	p, err := f.svc.SetPolicy(f.ctx, op, "spc_a", TypeQuestion, 1, base)
	must(t, err)
	if p.Version != 2 {
		t.Fatalf("updated %+v", p)
	}
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", TypeTodo, 3, base)
	code(t, err, apperr.CodePrecondition)
	for name, bad := range map[string]PolicyFields{
		"type":       base, // checked below with a bad type
		"reminders":  {MaxReminders: 0, RequesterEnded: EndedReroute, HoldMinutes: 60, RatePerRunHour: 1, RatePerSpaceHour: 1},
		"escalation": {MaxReminders: 1, EscalateAfterMinutes: 5, RequesterEnded: EndedReroute, HoldMinutes: 60, RatePerRunHour: 1, RatePerSpaceHour: 1},
		"ended":      {MaxReminders: 1, RequesterEnded: "wake", HoldMinutes: 60, RatePerRunHour: 1, RatePerSpaceHour: 1},
		"rate":       {MaxReminders: 1, RequesterEnded: EndedReroute, HoldMinutes: 60, RatePerRunHour: 0, RatePerSpaceHour: 1},
		"role":       {MaxReminders: 1, RequesterEnded: EndedReroute, HoldMinutes: 60, SuccessorRole: "Bad Role", RatePerRunHour: 1, RatePerSpaceHour: 1},
		"priority":   {MaxReminders: 1, EscalateAfterMinutes: 5, EscalatePriority: "low", RequesterEnded: EndedReroute, HoldMinutes: 60, RatePerRunHour: 1, RatePerSpaceHour: 1},
	} {
		typ := TypeTodo
		if name == "type" {
			typ = "poll"
		}
		_, err := f.svc.SetPolicy(f.ctx, op, "spc_a", typ, 0, bad)
		if apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Errorf("%s: %v", name, err)
		}
	}
	assignee := base
	assignee.EscalateAfterMinutes, assignee.EscalateAssignee = 5, "usr_ghost"
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_a", TypeTodo, 0, assignee)
	code(t, err, apperr.CodeInvalid)
	_, err = f.svc.SetPolicy(f.ctx, op, "spc_missing", TypeTodo, 0, base)
	code(t, err, apperr.CodeNotFound)
	_, err = f.svc.Policies(f.ctx, "spc_missing")
	code(t, err, apperr.CodeNotFound)
	must(t, f.svc.DeletePolicy(f.ctx, op, "spc_a", TypeQuestion))
	code(t, f.svc.DeletePolicy(f.ctx, op, "spc_a", TypeQuestion), apperr.CodeNotFound)
	if e := eff("spc_a"); e[TypeQuestion].Source != "default:all" {
		t.Fatalf("after delete %+v", e)
	}
	if d, _ := f.svc.Policies(f.ctx, ""); len(d) != 1 || d[0].ActionType != "all" || d[0].SpaceID != "" {
		t.Fatalf("defaults %+v", d)
	}
	// Recover is idempotent and fills schedules missing on actions from before reminders existed.
	f.exec(`UPDATE human_actions SET next_reminder_at = NULL`)
	must(t, f.svc.Recover(f.ctx))
	must(t, f.svc.Recover(f.ctx))
	if g := f.get(a.ID); g.NextReminderAt == nil {
		t.Fatalf("recover %+v", g)
	}
}

// SM-HUM-011: rate limits per requester run and per space, by policy, with an error that says waiting is enough.
func TestRateLimits(t *testing.T) {
	f := newFx(t)
	f.policy("spc_a", TypeTodo, PolicyFields{MaxReminders: 1, RequesterEnded: EndedReroute, HoldMinutes: 60, WakeIdle: true, RatePerRunHour: 2, RatePerSpaceHour: 3})
	f.create(CreateInput{Type: TypeTodo, Title: "1"})
	f.create(CreateInput{Type: TypeTodo, Title: "2"})
	_, _, err := f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "3", Requester: agent})
	code(t, err, apperr.CodeRateLimited)
	if !strings.Contains(err.Error(), "you are notified") || !strings.Contains(err.Error(), "space:todo") {
		t.Fatalf("message %v", err)
	}
	f.create(question("other types have their own limit"))
	other := Requester{Kind: "agent", ID: "ins_2", Role: "worker", Run: "run_2"}
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "4", Requester: other})
	must(t, err)
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "5", Requester: other})
	code(t, err, apperr.CodeRateLimited) // the space's limit
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "op", Requester: Requester{Kind: "operator", ID: "usr_op"}})
	must(t, err) // people are not rate limited
	f.clk.Advance(time.Hour)
	_, _, err = f.svc.Create(f.ctx, op, "spc_a", CreateInput{Type: TypeTodo, Title: "later", Requester: agent})
	must(t, err)
}

// SM-HUM-011: one timer for the earliest deadline, woken by Kick; extra deadlines (approvals) share it.
func TestDeadlineLoop(t *testing.T) {
	f := newFx(t)
	if next, err := f.svc.NextDeadline(f.ctx); err != nil || !next.IsZero() {
		t.Fatalf("empty %v %v", next, err)
	}
	if d := f.svc.untilNext(f.ctx, nil); d != idleWait {
		t.Fatalf("idle wait %v", d)
	}
	exp := f.clk.Now().Add(time.Hour)
	a := f.create(CreateInput{Type: TypeMessage, Title: "m", ExpiresAt: &exp})
	if next, _ := f.svc.NextDeadline(f.ctx); !next.Equal(exp) {
		t.Fatalf("next %v", next)
	}
	extraAt := f.clk.Now().Add(time.Minute)
	if d := f.svc.untilNext(f.ctx, []Deadline{{Next: func(context.Context) (time.Time, error) { return extraAt, nil }}}); d != time.Minute {
		t.Fatalf("extra wait %v", d)
	}
	calls := make(chan struct{}, 10)
	extra := Deadline{Name: "extra", Next: func(context.Context) (time.Time, error) { return time.Time{}, nil },
		Due: func(context.Context) (int, error) { calls <- struct{}{}; return 0, nil }}
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { f.svc.Run(ctx, extra); close(done) }()
	<-calls // the first round runs at once
	f.clk.Advance(2 * time.Hour)
	f.svc.Kick()
	deadline := time.Now().Add(5 * time.Second)
	for f.get(a.ID).Status != Expired {
		if time.Now().After(deadline) {
			t.Fatal("not expired after a kick")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	f.svc.Kick() // never blocks
	f.svc.Kick()
	_ = json.RawMessage{}
}

// ADR-0056: with control auto-start on, a response whose run is gone goes to control even while no control runs (it
// is started for it), when the space can start one; with auto-start off, or with no control to start, to the operators.
func TestResponsesGoToAControlThatCanBeStarted(t *testing.T) {
	f := newFx(t)
	on := false
	f.svc.ControlAutostart = func(_ context.Context, _ *db.Queries, spaceID string) bool { return on && spaceID == "spc_a" }
	ask := func(title string) *Action {
		return f.create(CreateInput{Type: TypeQuestion, Title: title, Requester: Requester{Kind: "agent", ID: "ins_2", Role: "worker", Run: "run_gone"}})
	}

	// Auto-start on, but the space never had control and has no control role: nothing can take it.
	on = true
	if n := f.answer(ask("a")).Notifications[0]; n.TargetKind != TargetOperators {
		t.Fatalf("no control to start %+v", n)
	}
	// A control ran once and was stopped (an idle stop): auto-start off keeps it with the operators.
	f.run("run_ctl", "control", "control", "ins_c", "running", "running")
	f.end("run_ctl")
	on = false
	if n := f.answer(ask("b")).Notifications[0]; n.TargetKind != TargetOperators {
		t.Fatalf("auto-start off %+v", n)
	}
	// Auto-start on: it waits for control, pending and waking, which starts control for it.
	on = true
	n := f.answer(ask("c")).Notifications[0]
	if n.TargetKind != TargetControl || n.RouteReason != ReasonRequesterEnded || n.OriginalRun != "run_gone" {
		t.Fatalf("to a stopped control %+v", n)
	}
	if f.count(`SELECT count(*) FROM human_notifications WHERE id = '`+n.ID+`' AND state = 'pending' AND wake = 1`) != 1 {
		t.Fatal("not pending and waking")
	}
}
