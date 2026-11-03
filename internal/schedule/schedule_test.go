package schedule

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/attachments"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/inbox"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t     *testing.T
	ctx   context.Context
	s     *store.Store
	clk   *clock.Fake
	svc   *Service
	inbox *inbox.Service
	queue *queue.Service
	kicks int
}

func newFx(t *testing.T, at time.Time) *fx {
	t.Helper()
	clk := clock.NewFake(at)
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	att := attachments.New(s, attachments.NewLocal(t.TempDir()), aw, attachments.Options{})
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, inbox: inbox.New(s, aw, ch, att), queue: queue.New(s, aw, ch)}
	f.svc = New(s, aw, ch, f.inbox, f.queue, clk, nil)
	f.svc.Kick = func() { f.kicks++ }
	must(t, s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range []string{
			`INSERT INTO nodes (id, name, state, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
			`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`,
		} {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return err
			}
		}
		return nil
	}))
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fx) items(space string) []*inbox.Item {
	p, err := f.inbox.List(f.ctx, space, "", "", 100)
	must(f.t, err)
	return p.Items
}

func (f *fx) occurrences(id string) []Occurrence {
	o, err := f.svc.Occurrences(f.ctx, "spc_a", id, 100)
	must(f.t, err)
	return o
}

func (f *fx) due() int {
	f.t.Helper()
	n, err := f.svc.Due(f.ctx)
	must(f.t, err)
	return n
}

// SM-LIMIT-003: a cron schedule runs in its IANA time zone, across daylight-saving changes.
func TestCronInTimeZone(t *testing.T) {
	f := newFx(t, time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC))
	got, err := f.svc.Preview(Input{Kind: KindCron, Cron: "0 9 * * *", Timezone: "Europe/Vilnius"}, 4)
	must(t, err)
	// Vilnius leaves summer time (UTC+3) on 25 October 2026: 09:00 local is 06:00 UTC before and 07:00 UTC after.
	want := []string{"2026-10-24T06:00:00Z", "2026-10-25T07:00:00Z", "2026-10-26T07:00:00Z", "2026-10-27T07:00:00Z"}
	for i, w := range want {
		if got[i].Format(time.RFC3339) != w {
			t.Fatalf("occurrence %d: %s, want %s (all %v)", i, got[i].Format(time.RFC3339), w, got)
		}
	}
	for name, in := range map[string]Input{
		"bad zone":     {Kind: KindCron, Cron: "0 9 * * *", Timezone: "Mars/Olympus"},
		"local zone":   {Kind: KindCron, Cron: "0 9 * * *", Timezone: "Local"},
		"tz in spec":   {Kind: KindCron, Cron: "CRON_TZ=UTC 0 9 * * *"},
		"every":        {Kind: KindCron, Cron: "@every 5m"},
		"never":        {Kind: KindCron, Cron: "0 0 30 2 *"},
		"syntax":       {Kind: KindCron, Cron: "61 * * * *"},
		"short":        {Kind: KindInterval, IntervalSeconds: 30},
		"cron+seconds": {Kind: KindCron, Cron: "@daily", IntervalSeconds: 60},
		"kind":         {Kind: "sometimes"},
		"catch up":     {Kind: KindInterval, IntervalSeconds: 60, Missed: MissedCatchUp, CatchUpMax: 500},
		"missed":       {Kind: KindInterval, IntervalSeconds: 60, Missed: "maybe"},
		"target":       {Kind: KindInterval, IntervalSeconds: 60, Target: "email"},
		"assignee":     {Kind: KindInterval, IntervalSeconds: 60, Target: TargetInbox, AssigneeRole: "worker"},
		"priority":     {Kind: KindInterval, IntervalSeconds: 60, Priority: "asap"},
	} {
		if _, err := f.svc.Preview(in, 1); apperr.CodeOf(err) != apperr.CodeInvalid {
			t.Fatalf("%s: %v", name, err)
		}
	}
	got, err = f.svc.Preview(Input{Kind: KindInterval, IntervalSeconds: 3600}, 2)
	must(t, err)
	if !got[0].Equal(f.clk.Now().Add(time.Hour)) || !got[1].Equal(f.clk.Now().Add(2*time.Hour)) {
		t.Fatalf("interval %v", got)
	}
}

// SM-LIMIT-003: each occurrence is handled once and its key deduplicates what it creates, also when it is handled
// again after a crash; a single timer knows the earliest occurrence of every schedule.
func TestOccurrenceOnceWithDedup(t *testing.T) {
	f := newFx(t, time.Date(2026, 11, 3, 8, 30, 0, 0, time.UTC))
	s, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "standup", Kind: KindCron, Cron: "0 9 * * 1-5", Timezone: "UTC", Target: TargetInbox,
		Title: "Daily check", Body: "Review open tasks."})
	must(t, err)
	if s.NextAt == nil || s.NextAt.Format(time.RFC3339) != "2026-11-03T09:00:00Z" || len(s.Upcoming) != 3 || f.kicks != 1 {
		t.Fatalf("created %+v kicks %d", s, f.kicks)
	}
	other, err := f.svc.Create(f.ctx, admin, "spc_b", Input{Name: "hourly", Kind: KindInterval, IntervalSeconds: 3600, Target: TargetInbox, Title: "Tick"})
	must(t, err)
	if next, _ := f.svc.Next(f.ctx); !next.Equal(*s.NextAt) || !other.NextAt.After(next) {
		t.Fatalf("earliest %v, want %v (other %v)", next, s.NextAt, other.NextAt)
	}
	if _, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "standup", Kind: KindInterval, IntervalSeconds: 60, Target: TargetInbox, Title: "x"}); apperr.CodeOf(err) != apperr.CodeAlreadyExists {
		t.Fatalf("duplicate name: %v", err)
	}
	if n := f.due(); n != 0 {
		t.Fatalf("nothing due yet, created %d", n)
	}
	f.clk.Set(time.Date(2026, 11, 3, 9, 0, 1, 0, time.UTC))
	if n := f.due(); n != 1 {
		t.Fatalf("due: %d", n)
	}
	items := f.items("spc_a")
	if len(items) != 1 || items[0].SourceKind != "schedule" || items[0].SourceID != s.ID || items[0].Title != "Daily check" ||
		items[0].DedupKey != "schedule:"+s.ID+":"+itoa(int(s.NextAt.Unix())) || items[0].Trusted["late"] != false {
		t.Fatalf("item %+v", items)
	}
	if n := f.due(); n != 0 {
		t.Fatalf("handled twice: %d", n)
	}
	got, _ := f.svc.Get(f.ctx, "spc_a", s.ID)
	if got.NextAt.Format(time.RFC3339) != "2026-11-04T09:00:00Z" || got.LastOutcome != "created" || got.LastTargetID != items[0].ID {
		t.Fatalf("after: %+v", got)
	}
	// A crash after the item was created but before the schedule moved on: the same occurrence again is a duplicate.
	must(t, f.s.W().TxNamed(f.ctx, "rewind", func(tx *store.Tx) error {
		if _, err := tx.SQL.ExecContext(f.ctx, `UPDATE space_schedules SET next_at = ?, last_at = NULL, last_outcome = '', last_target_id = '' WHERE id = ?`, time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC).UnixMicro(), s.ID); err != nil {
			return err
		}
		_, err := tx.SQL.ExecContext(f.ctx, `DELETE FROM schedule_occurrences WHERE schedule_id = ?`, s.ID)
		return err
	}))
	if n := f.due(); n != 0 || len(f.items("spc_a")) != 1 {
		t.Fatalf("replayed occurrence created %d (items %d)", n, len(f.items("spc_a")))
	}
	if o := f.occurrences(s.ID); len(o) != 1 || o[0].Outcome != "duplicate" || o[0].TargetID != items[0].ID {
		t.Fatalf("occurrences %+v", o)
	}
	// Disabled schedules have no next occurrence; deleting one keeps what it created.
	cur, _ := f.svc.Get(f.ctx, "spc_a", s.ID)
	off := false
	upd, err := f.svc.Update(f.ctx, admin, "spc_a", s.ID, cur.Version, Input{Name: "standup", Enabled: &off, Kind: KindCron, Cron: "0 9 * * 1-5", Target: TargetInbox, Title: "Daily check"})
	must(t, err)
	if upd.NextAt != nil || upd.Enabled {
		t.Fatalf("disabled %+v", upd)
	}
	if _, err := f.svc.Update(f.ctx, admin, "spc_a", s.ID, cur.Version, Input{Name: "standup", Kind: KindCron, Cron: "@daily", Target: TargetInbox, Title: "x"}); apperr.CodeOf(err) != apperr.CodePrecondition {
		t.Fatalf("stale update: %v", err)
	}
	must(t, f.svc.Delete(f.ctx, admin, "spc_a", s.ID))
	if len(f.items("spc_a")) != 1 {
		t.Fatal("deleting the schedule removed its item")
	}
	if err := f.svc.Delete(f.ctx, admin, "spc_a", s.ID); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("delete twice: %v", err)
	}
}

// SM-LIMIT-003: occurrences missed while central was stopped follow the schedule's policy: skip records them, run_once
// runs the latest once, catch_up runs a bounded number, oldest first; every missed occurrence is accounted for.
func TestMissedRunPolicies(t *testing.T) {
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		policy  string
		created int
		late    int
	}{{MissedSkip, 0, 0}, {MissedRunOnce, 1, 1}, {MissedCatchUp, 3, 3}} {
		t.Run(tc.policy, func(t *testing.T) {
			f := newFx(t, start)
			s, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "hourly", Kind: KindInterval, IntervalSeconds: 3600, Missed: tc.policy, CatchUpMax: 3,
				SkipIfOpen: new(bool), Target: TargetInbox, Title: "Tick"})
			must(t, err)
			// Central was down from 00:00 to 05:30: occurrences at 01:00 … 05:00 were missed.
			f.clk.Set(start.Add(5*time.Hour + 30*time.Minute))
			if n := f.due(); n != tc.created {
				t.Fatalf("created %d, want %d", n, tc.created)
			}
			late := 0
			for _, it := range f.items("spc_a") {
				if it.Trusted["late"] == true {
					late++
				}
			}
			if late != tc.late || len(f.items("spc_a")) != tc.created {
				t.Fatalf("late items %d of %d", late, len(f.items("spc_a")))
			}
			o := f.occurrences(s.ID)
			var missed *Occurrence
			for i := range o {
				if o[i].Outcome == "missed" {
					missed = &o[i]
				}
			}
			if wantMissed := 5 - tc.created; missed == nil || !strings.HasPrefix(missed.Detail, itoa(wantMissed)+" occurrence(s) missed") {
				t.Fatalf("missed record %+v (all %+v)", missed, o)
			}
			if tc.policy == MissedCatchUp {
				// The latest three, oldest first: 03:00, 04:00, 05:00.
				items := f.items("spc_a")
				if !strings.HasSuffix(items[len(items)-1].DedupKey, itoa(int(start.Add(3*time.Hour).Unix()))) {
					t.Fatalf("catch-up order %v", items[len(items)-1].DedupKey)
				}
			}
			got, _ := f.svc.Get(f.ctx, "spc_a", s.ID)
			if !got.NextAt.Equal(start.Add(6 * time.Hour)) {
				t.Fatalf("next %v", got.NextAt)
			}
			// The next occurrence runs on time, not late.
			f.clk.Set(start.Add(6*time.Hour + time.Second))
			if n := f.due(); n != 1 {
				t.Fatalf("on time: %d", n)
			}
		})
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// SM-LIMIT-003: skip_if_open does not pile up work: an occurrence is skipped while what the previous one created is
// still open; task schedules create ready tasks with the schedule as their origin.
func TestSkipWhileOpenAndTasks(t *testing.T) {
	start := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
	f := newFx(t, start)
	s, err := f.svc.Create(f.ctx, admin, "spc_a", Input{Name: "deps", Kind: KindInterval, IntervalSeconds: 3600, Target: TargetTask, Title: "Update dependencies",
		Body: "Run the updater.", Priority: "high", AssigneeRole: "worker"})
	must(t, err)
	f.clk.Set(start.Add(time.Hour))
	if n := f.due(); n != 1 {
		t.Fatalf("first: %d", n)
	}
	tasks, _, err := f.queue.List(f.ctx, "spc_a")
	must(t, err)
	if len(tasks) != 1 || tasks[0].OriginKind != "schedule" || tasks[0].OriginID != s.ID || tasks[0].Status != "ready" || tasks[0].Priority != "high" || tasks[0].AssigneeRole != "worker" {
		t.Fatalf("task %+v", tasks)
	}
	f.clk.Set(start.Add(2 * time.Hour))
	if n := f.due(); n != 0 {
		t.Fatalf("created while the previous task is open: %d", n)
	}
	if o := f.occurrences(s.ID); o[0].Outcome != "skipped" {
		t.Fatalf("occurrence %+v", o[0])
	}
	_, err = f.queue.SetStatus(f.ctx, admin, "spc_a", tasks[0].ID, tasks[0].Version, queue.StatusInput{Status: "cancelled", Reason: "not needed"})
	must(t, err)
	f.clk.Set(start.Add(3 * time.Hour))
	if n := f.due(); n != 1 {
		t.Fatalf("after the previous task ended: %d", n)
	}
	// Archived spaces are not scheduled; their occurrences wait for the missed-run policy.
	must(t, f.s.W().TxNamed(f.ctx, "archive", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, `UPDATE spaces SET state = 'archived' WHERE id = 'spc_a'`)
		return err
	}))
	if next, _ := f.svc.Next(f.ctx); !next.IsZero() {
		t.Fatalf("archived space scheduled at %v", next)
	}
}
