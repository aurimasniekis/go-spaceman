package messages

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
)

var ctrlFrom = From{Kind: "run", ID: ctrlRun, Label: "control (control)"}

// ADR-0065: scheduling bounds and who may be scheduled to.
func TestScheduleBounds(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	now := f.clk.Now()
	for _, in := range []SendInput{
		{To: "control", Subject: "x", Delay: 59 * time.Second},
		{To: "control", Subject: "x", Delay: MaxDelay + time.Second},
		{To: "control", Subject: "x", DeliverAt: now.Add(-time.Hour)},
		{To: "control", Subject: "x", DeliverAt: now.Add(31 * 24 * time.Hour)},
		{To: "control", Subject: "x", Delay: time.Hour, DeliverAt: now.Add(time.Hour)},
		{To: "role:implementor", Subject: "x", Delay: time.Hour},
		{To: "operators", Subject: "x", Delay: time.Hour},
		{To: "control", Subject: "x", Kind: "reply", ReplyTo: "msg_x", Delay: time.Hour},
	} {
		_, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, in)
		code(t, err, apperr.CodeInvalid)
	}
	for _, in := range []SendInput{
		{To: "control", Subject: "min", Delay: MinDelay},
		{To: "control", Subject: "max", Delay: MaxDelay},
		{To: "control", Subject: "at", DeliverAt: now.Add(6 * time.Hour)},
	} {
		m, created, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, in)
		must(t, err)
		if !created || m.State != Stored || m.DeliverAt == nil || m.AvailableAt != nil {
			t.Fatalf("%s: %+v", in.Subject, m)
		}
	}
	l, err := f.svc.Scheduled(f.ctx, "spc_a")
	must(t, err)
	if len(l) != 3 || l[0].Subject != "min" || l[1].Subject != "at" || l[2].Subject != "max" {
		t.Fatalf("scheduled %+v", l)
	}
}

// ADR-0065: a scheduled message is invisible until due (no fetch, no notice, not waiting work, no control wanted);
// when due it wakes or asks for control and is noticed exactly once, also to the control run that scheduled it.
func TestScheduledDelivery(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	var wanted []string
	f.n.ControlWanted = func(_ context.Context, spaceID string) { wanted = append(wanted, spaceID) }
	f.exec(`UPDATE runs SET status = 'idle' WHERE id = '` + ctrlRun + `'`)
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "Check the deploy", Delay: 6 * time.Hour})
	must(t, err)
	if next := f.n.nextSweep(f.ctx); next != sweepEvery {
		t.Fatalf("next sweep %s while due in 6h", next)
	}
	check := func(label string, wantWaiting bool) {
		t.Helper()
		if w, err := f.svc.Waiting(f.ctx, "spc_a", "run_01ffffffffffffffffffffffff", "control", true); err != nil || w != wantWaiting {
			t.Fatalf("%s: waiting %v %v", label, w, err)
		}
	}
	check("before due", false)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrlRun) != 0 {
		t.Fatalf("noticed before due: %v", f.q.notices)
	}
	in, err := f.svc.Fetch(f.ctx, ctrl)
	must(t, err)
	if len(in.Messages) != 0 {
		t.Fatalf("fetched before due: %+v", in.Messages)
	}
	// No control is live: still nothing is wanted before it is due.
	f.exec(`UPDATE runs SET desired = 'stopped', observed = 'exited' WHERE id = '` + ctrlRun + `'`)
	must(t, f.n.Sweep(f.ctx))
	if len(wanted) != 0 {
		t.Fatalf("control wanted before due: %v", wanted)
	}
	f.clk.Advance(6*time.Hour - 30*time.Second)
	if next := f.n.nextSweep(f.ctx); next < 30*time.Second || next > 31*time.Second {
		t.Fatalf("next sweep %s, want just after 30s", next)
	}
	f.clk.Advance(30 * time.Second)
	check("due", true)
	must(t, f.n.Sweep(f.ctx))
	if len(wanted) != 1 || wanted[0] != "spc_a" {
		t.Fatalf("due: wanted %v", wanted)
	}
	// A control started for it is noticed once.
	f.exec(`UPDATE runs SET desired = 'running', observed = 'running', started_at = ` + fmt.Sprint(f.clk.Now().UnixMicro()) + ` WHERE id = '` + ctrlRun + `'`)
	must(t, f.n.Sweep(f.ctx))
	f.clk.Advance(2 * time.Minute)
	must(t, f.n.Sweep(f.ctx))
	if f.q.count(ctrlRun) != 1 {
		t.Fatalf("notices %d, want 1", f.q.count(ctrlRun))
	}
	in, err = f.svc.Fetch(f.ctx, ctrl)
	must(t, err)
	if len(in.Messages) != 1 || in.Messages[0].ID != m.ID || in.Messages[0].DeliverAt == nil || in.Messages[0].State != Available {
		t.Fatalf("delivered %+v", in.Messages)
	}
	if l, _ := f.svc.Scheduled(f.ctx, "spc_a"); len(l) != 0 {
		t.Fatalf("still scheduled %+v", l)
	}
	_, err = f.svc.Cancel(f.ctx, op, "spc_a", m.ID)
	code(t, err, apperr.CodeConflict)
}

// ADR-0065: cancelling before due means it is never delivered; cap and dedup across control runs.
func TestScheduleCancelCapDedup(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "Later", Delay: time.Hour, DedupKey: "check-ci"})
	must(t, err)
	// A later control run re-scheduling with the same key gets the pending one back.
	again, created, err := f.svc.Send(f.ctx, op, "spc_a", From{Kind: "run", ID: "run_01ffffffffffffffffffffffff"},
		SendInput{To: "control", Subject: "Later", Delay: 2 * time.Hour, DedupKey: "check-ci"})
	must(t, err)
	if created || again.ID != m.ID {
		t.Fatalf("dedup: created %v, %s vs %s", created, again.ID, m.ID)
	}
	_, err = f.svc.Cancel(f.ctx, op, "spc_b", m.ID)
	code(t, err, apperr.CodeNotFound)
	c, err := f.svc.Cancel(f.ctx, op, "spc_a", m.ID)
	must(t, err)
	if c.State != Resolved || c.Resolution != CancelledResolution || c.DeliverAt == nil {
		t.Fatalf("cancelled %+v", c)
	}
	_, err = f.svc.Cancel(f.ctx, op, "spc_a", m.ID)
	code(t, err, apperr.CodeConflict)
	f.clk.Advance(2 * time.Hour)
	if in, _ := f.svc.Fetch(f.ctx, ctrl); len(in.Messages) != 0 {
		t.Fatalf("cancelled message delivered: %+v", in.Messages)
	}
	// A plain message cannot be cancelled.
	plain, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "now"})
	must(t, err)
	_, err = f.svc.Cancel(f.ctx, op, "spc_a", plain.ID)
	code(t, err, apperr.CodeConflict)

	for i := range MaxScheduled {
		_, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: fmt.Sprint("r", i), Delay: time.Hour})
		must(t, err)
	}
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "one too many", Delay: time.Hour})
	code(t, err, apperr.CodeRateLimited)
	// Due ones no longer count against the cap.
	f.clk.Advance(time.Hour)
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "room again", Delay: time.Hour})
	must(t, err)
}

// ADR-0065 (review F1): a scheduled send whose key the sender already used for a message that is not a pending
// reminder is a conflict, never a silent no-op returning the old message.
func TestScheduleDedupReuseConflicts(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	// Reused after delivery: the same control run schedules "k", it is delivered, then it schedules "k" again.
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "Check CI", Delay: time.Hour, DedupKey: "k"})
	must(t, err)
	f.clk.Advance(time.Hour)
	in, err := f.svc.Fetch(f.ctx, ctrl)
	must(t, err)
	if len(in.Messages) != 1 || in.Messages[0].ID != m.ID {
		t.Fatalf("delivered %+v", in.Messages)
	}
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "Check CI again", Delay: time.Hour, DedupKey: "k"})
	code(t, err, apperr.CodeConflict)
	if !strings.Contains(err.Error(), m.ID) {
		t.Fatalf("conflict does not name %s: %v", m.ID, err)
	}
	// A plain message sent earlier with key "p".
	plain, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "now", DedupKey: "p"})
	must(t, err)
	_, _, err = f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "later", Delay: time.Hour, DedupKey: "p"})
	code(t, err, apperr.CodeConflict)
	if !strings.Contains(err.Error(), plain.ID) {
		t.Fatalf("conflict does not name %s: %v", plain.ID, err)
	}
	if l, _ := f.svc.Scheduled(f.ctx, "spc_a"); len(l) != 0 {
		t.Fatalf("conflicting sends scheduled %+v", l)
	}
	// A plain send repeating a key still returns the original (SM-COMM-004), and a new key schedules.
	again, created, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "now", DedupKey: "p"})
	must(t, err)
	if created || again.ID != plain.ID {
		t.Fatalf("plain dedup: created %v, %s vs %s", created, again.ID, plain.ID)
	}
	_, created, err = f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "Check CI again", Delay: time.Hour, DedupKey: "k2"})
	must(t, err)
	if !created {
		t.Fatal("new key not created")
	}
}

// ADR-0065 (review F5): a reminder that fell due while central was down is delivered by the first sweep of the new
// notifier, without waiting for a timer: Run sweeps before it arms one, and the queries compare against now.
func TestReminderDueWhileDownDeliveredOnStart(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	f.exec(`UPDATE runs SET status = 'idle' WHERE id = '` + ctrlRun + `'`)
	m, _, err := f.svc.Send(f.ctx, op, "spc_a", ctrlFrom, SendInput{To: "control", Subject: "Check the deploy", Delay: time.Hour})
	must(t, err)
	// Central is down past the due time; a fresh service and notifier over the same database stand for the restart.
	f.clk.Advance(3 * time.Hour)
	svc := New(f.s, audit.NewWriter(f.kr), changefeed.New(nil))
	q := &fakeQueue{notices: map[string][]string{}}
	n := NewNotifier(svc, q, nil, f.clk)
	ctx, cancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() { defer close(done); n.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for q.count(ctrlRun) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first sweep after the restart did not notice the due reminder")
		}
		time.Sleep(10 * time.Millisecond)
	}
	in, err := svc.Fetch(f.ctx, ctrl)
	must(t, err)
	if len(in.Messages) != 1 || in.Messages[0].ID != m.ID || in.Messages[0].State != Available {
		t.Fatalf("delivered %+v", in.Messages)
	}
	if q.count(ctrlRun) != 1 {
		t.Fatalf("notices %d, want 1", q.count(ctrlRun))
	}
}

// ADR-0065: the scheduling tools are control's; workers cannot schedule, list or cancel.
func TestScheduleTools(t *testing.T) {
	t.Parallel()
	f := newFx(t)
	r := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc)}})
	pc := &fleet.RunPrincipal{RunID: ctrlRun, SpaceID: "spc_a", Kind: "control", Role: "control"}
	pw := &fleet.RunPrincipal{RunID: wrkRun, SpaceID: "spc_a", Kind: "worker", Role: "implementor"}
	call := func(p *fleet.RunPrincipal, tool string, args any, out any) error {
		b, _ := json.Marshal(args)
		raw, err := r.Dispatch(f.ctx, p, tool, b)
		if err == nil && out != nil {
			must(t, json.Unmarshal(raw, out))
		}
		return err
	}
	code(t, call(pw, "spaceman_message_send", map[string]any{"to": "control", "subject": "x", "delay": "1h"}, nil), apperr.CodeForbidden)
	code(t, call(pc, "spaceman_message_send", map[string]any{"to": "control", "subject": "x", "delay": "soon"}, nil), apperr.CodeInvalid)
	code(t, call(pc, "spaceman_message_send", map[string]any{"to": "control", "subject": "x", "deliver_at": "tomorrow"}, nil), apperr.CodeInvalid)
	code(t, call(pc, "spaceman_message_send", map[string]any{"to": "control", "subject": "x", "delay": "30s"}, nil), apperr.CodeInvalid)
	var a, b Message
	must(t, call(pc, "spaceman_message_send", map[string]any{"to": "control", "subject": "In 6h", "delay": "6h"}, &a))
	at := f.clk.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	must(t, call(pc, "spaceman_message_send", map[string]any{"to": "control", "subject": "At", "deliver_at": at}, &b))
	if a.DeliverAt == nil || !a.DeliverAt.Equal(f.clk.Now().Add(6*time.Hour)) || b.DeliverAt == nil || b.DeliverAt.UTC().Format(time.RFC3339) != at {
		t.Fatalf("scheduled %+v / %+v", a.DeliverAt, b.DeliverAt)
	}
	var list struct {
		Items []Message `json:"items"`
		Max   int       `json:"max"`
	}
	must(t, call(pc, "spaceman_message_scheduled", nil, &list))
	if len(list.Items) != 2 || list.Items[0].ID != b.ID || list.Max != MaxScheduled {
		t.Fatalf("list %+v", list)
	}
	for _, tool := range []string{"spaceman_message_scheduled", "spaceman_message_cancel"} {
		if err := call(pw, tool, map[string]any{"message_id": a.ID}, nil); err == nil {
			t.Fatalf("a worker called %s", tool)
		}
	}
	var c Message
	must(t, call(pc, "spaceman_message_cancel", map[string]any{"message_id": a.ID}, &c))
	if c.State != Resolved {
		t.Fatalf("cancel %+v", c)
	}
}

// The agent tool guide documents every messaging tool (agenttools.TestGuideListsEveryTool covers only the registry's
// own tools; these are registered as an extra).
func TestGuideListsMessageTools(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("../../site/guide/agent-tools.md")
	must(t, err)
	r := agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(nil)}})
	seen := 0
	for _, kind := range []string{agenttools.KindControl, agenttools.KindWorker, agenttools.KindKB, agenttools.KindDreamer, agenttools.KindDiagnostic} {
		for _, x := range r.Catalog(kind) {
			if !strings.HasPrefix(x.Name, "spaceman_message") {
				continue
			}
			seen++
			if !strings.Contains(string(b), "`"+x.Name+"`") {
				t.Errorf("site/guide/agent-tools.md does not document %s", x.Name)
			}
		}
	}
	if seen < 7 {
		t.Fatalf("only %d messaging tools in the catalogs", seen)
	}
}
