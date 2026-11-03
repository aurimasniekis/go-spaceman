package retention

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/limits"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t           *testing.T
	ctx         context.Context
	s           *store.Store
	clk         *clock.Fake
	aw          *audit.Writer
	lim         *limits.Service
	svc         *Service
	mu          sync.Mutex
	recordings  []string
	transcripts []string
}

func newFx(t *testing.T, auditDays int64) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, aw: audit.NewWriter(kr)}
	f.lim = limits.New(s, f.aw, changefeed.New(nil), config.Default().LimitValues(), clk, nil)
	f.svc = New(s, f.aw, changefeed.New(nil), f.lim, Options{AuditDays: auditDays, Clock: clk,
		DeleteRecording: func(_ context.Context, _ audit.Actor, _, runID string, _ map[string]any) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.recordings = append(f.recordings, runID)
			return f.s.W().TxNamed(f.ctx, "mark", func(tx *store.Tx) error {
				_, err := tx.SQL.ExecContext(f.ctx, `UPDATE runs SET recording_deleted_at = 1 WHERE id = ?`, runID)
				return err
			})
		},
		DeleteTranscript: func(_ context.Context, _ audit.Actor, _, runID string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.transcripts = append(f.transcripts, runID)
			return f.s.W().TxNamed(f.ctx, "mark", func(tx *store.Tx) error {
				_, err := tx.SQL.ExecContext(f.ctx, `UPDATE run_transcripts SET state = 'deleted' WHERE run_id = ?`, runID)
				return err
			})
		}})
	f.exec(`INSERT INTO nodes (id, name, state, capacity, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', 4, 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`,
		`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_b', 'bbb', 'B', 'nod_1', 'active', 'b', 0, 't', 0)`)
	return f
}

func (f *fx) exec(qs ...string) {
	f.t.Helper()
	must(f.t, f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		for _, q := range qs {
			if _, err := tx.SQL.ExecContext(f.ctx, q); err != nil {
				return fmt.Errorf("%s: %w", q, err)
			}
		}
		return nil
	}))
}

func (f *fx) count(q string, args ...any) int {
	f.t.Helper()
	var n int
	must(f.t, f.s.ReadDB().QueryRowContext(f.ctx, q, args...).Scan(&n))
	return n
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// seed creates, three days before now, data that retention may delete and data it must keep.
func (f *fx) seed() {
	old := f.clk.Now().Add(-72 * time.Hour).UnixMicro()
	recent := f.clk.Now().Add(-time.Hour).UnixMicro()
	run := func(id, space string, ended any) string {
		return fmt.Sprintf(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json,
			created_at, created_by, updated_at, ended_at) VALUES ('%s', '%s', 'nod_1', 1, 'worker', 'w', 'claude-code', 'ins', 'stopped', 'exited', '{}', %d, 't', 0, %v)`,
			id, space, old, ended)
	}
	ha := func(id, status, task, followUp string, at int64) string {
		return fmt.Sprintf(`INSERT INTO human_actions (id, space_id, type, title, requester_kind, rank, status, task_id, follow_up_of, created_at, created_by, updated_at)
			VALUES ('%s', 'spc_a', 'question', 'Q', 'operator', 1, '%s', '%s', '%s', %d, 't', %d)`, id, status, task, followUp, at, at)
	}
	item := func(id, status, linkedKind, linkedID string, at int64) string {
		return fmt.Sprintf(`INSERT INTO inbox_items (id, space_id, source_kind, title, content_type, status, linked_kind, linked_id, received_at, created_by, updated_at)
			VALUES ('%s', 'spc_a', 'operator', 'I', 'text/markdown', '%s', '%s', '%s', %d, 't', %d)`, id, status, linkedKind, linkedID, at, at)
	}
	att := func(id string) string {
		return fmt.Sprintf(`INSERT INTO attachments (id, space_id, sha256, size, mime, name, uploaded_by, created_at) VALUES ('%s', 'spc_a', '%s', 10, 'text/plain', 'a.txt', 't', %d)`,
			id, id, old)
	}
	ref := func(att, item string) string {
		return fmt.Sprintf(`INSERT INTO attachment_refs (attachment_id, owner_kind, owner_id, created_at) VALUES ('%s', 'inbox_item', '%s', %d)`, att, item, old)
	}
	f.exec(
		run("run_old_a", "spc_a", old), run("run_active_a", "spc_a", "NULL"), run("run_recent_a", "spc_a", recent), run("run_old_b", "spc_b", old),
		fmt.Sprintf(`INSERT INTO run_transcripts (run_id, space_id, size, state, created_at, updated_at) VALUES ('run_old_a', 'spc_a', 1, 'stored', %d, %d)`, old, old),
		fmt.Sprintf(`INSERT INTO run_transcripts (run_id, space_id, size, state, created_at, updated_at) VALUES ('run_recent_a', 'spc_a', 1, 'stored', %d, %d)`, recent, recent),
		fmt.Sprintf(`INSERT INTO tasks (id, space_id, title, rank, status, created_at, created_by, updated_at) VALUES ('tsk_open', 'spc_a', 'T', 1, 'running', %d, 't', %d)`, old, old),
		fmt.Sprintf(`INSERT INTO tasks (id, space_id, title, rank, status, created_at, created_by, updated_at) VALUES ('tsk_done', 'spc_a', 'T', 2, 'done', %d, 't', %d)`, old, old),
		ha("hum_gone", "resolved", "", "", old),
		ha("hum_done_task", "cancelled", "tsk_done", "", old),
		ha("hum_open_task", "resolved", "tsk_open", "", old),
		ha("hum_followed", "resolved", "", "", old), ha("hum_followup", "open", "", "hum_followed", old),
		ha("hum_undelivered", "expired", "", "", old),
		fmt.Sprintf(`INSERT INTO human_notifications (id, action_id, space_id, kind, state, created_at) VALUES ('hnt_1', 'hum_undelivered', 'spc_a', 'expired', 'pending', %d)`, old),
		ha("hum_blocker", "resolved", "", "", old),
		fmt.Sprintf(`INSERT INTO task_blockers (id, task_id, kind, ref_id, created_at, created_by) VALUES ('tbl_1', 'tsk_open', 'human_action', 'hum_blocker', %d, 't')`, old),
		ha("hum_recent", "resolved", "", "", recent),
		item("inb_done", "done", "", "", old), att("att_done"), ref("att_done", "inb_done"),
		item("inb_new", "new", "", "", old), att("att_open"), ref("att_open", "inb_new"),
		item("inb_task", "done", "task", "tsk_open", old), att("att_task"), ref("att_task", "inb_task"),
		att("att_shared"), ref("att_shared", "inb_done"), ref("att_shared", "inb_new"),
		att("att_unref"),
	)
}

// One pass deletes what is older than the space's retention and never what open work still uses; other spaces keep
// the installation default (keep). Every deletion is audited and in the change feed.
func TestPassDeletesOnlyFinishedExpiredData(t *testing.T) {
	f := newFx(t, 0)
	f.seed()
	_, err := f.lim.Set(f.ctx, admin, "spc_a", 0, limits.Input{Values: map[string]int64{limits.RetainRecordings: 1, limits.RetainTranscripts: 1,
		limits.RetainAttachments: 1, limits.RetainHumanActions: 1}})
	must(t, err)

	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.State != StateSucceeded || p.Trigger != TriggerManual || p.FinishedAt == nil {
		t.Fatalf("pass %+v", p)
	}
	want := Counts{KindRecordings: 1, KindTranscripts: 1, KindHumanActions: 2, KindAttachments: 1}
	for k, v := range want {
		if p.Deleted[k] != v {
			t.Fatalf("deleted %v, want %v", p.Deleted, want)
		}
	}
	if !slices.Equal(f.recordings, []string{"run_old_a"}) || !slices.Equal(f.transcripts, []string{"run_old_a"}) {
		t.Fatalf("recordings %v transcripts %v", f.recordings, f.transcripts)
	}
	for _, id := range []string{"hum_gone", "hum_done_task"} {
		if f.count(`SELECT count(*) FROM human_actions WHERE id = ?`, id) != 0 {
			t.Fatalf("%s not deleted", id)
		}
	}
	for _, id := range []string{"hum_open_task", "hum_followed", "hum_followup", "hum_undelivered", "hum_blocker", "hum_recent"} {
		if f.count(`SELECT count(*) FROM human_actions WHERE id = ?`, id) != 1 {
			t.Fatalf("%s was deleted although something open refers to it (or it is recent)", id)
		}
	}
	if f.count(`SELECT count(*) FROM attachments WHERE id = 'att_done'`) != 0 {
		t.Fatal("att_done not deleted")
	}
	for _, id := range []string{"att_open", "att_task", "att_shared", "att_unref"} {
		if f.count(`SELECT count(*) FROM attachments WHERE id = ?`, id) != 1 {
			t.Fatalf("%s deleted although an open item uses it (or nothing references it: collection's job)", id)
		}
	}
	if n := f.count(`SELECT count(*) FROM audit_events WHERE action IN ('human_action.deleted', 'attachment.deleted') AND actor_id = 'retention'`); n != 3 {
		t.Fatalf("%d deletion audit events, want 3", n)
	}
	if n := f.count(`SELECT count(*) FROM changes WHERE action = 'deleted' AND resource_type IN ('human_action', 'attachment') AND space_id = 'spc_a'`); n != 3 {
		t.Fatalf("%d deletion changes, want 3", n)
	}
	if n := f.count(`SELECT count(*) FROM audit_events WHERE action IN ('retention.started', 'retention.finished')`); n != 2 {
		t.Fatalf("%d pass audit events", n)
	}

	// A second pass finds nothing new.
	p, err = f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.Deleted[KindHumanActions]+p.Deleted[KindAttachments]+p.Deleted[KindRecordings]+p.Deleted[KindTranscripts] != 0 {
		t.Fatalf("second pass deleted %v", p.Deleted)
	}

	// Once the open item is finished and the period passed, its attachment goes too.
	f.exec(`UPDATE inbox_items SET status = 'done', updated_at = 0 WHERE id = 'inb_new'`)
	p, err = f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.Deleted[KindAttachments] != 2 {
		t.Fatalf("after finishing inb_new: %v", p.Deleted)
	}
}

// While a restore is not acknowledged, nothing is deleted and no pass is scheduled.
func TestNothingDeletedBeforeRestoreAcknowledged(t *testing.T) {
	f := newFx(t, 30)
	f.seed()
	_, err := f.lim.Set(f.ctx, admin, "spc_a", 0, limits.Input{Values: map[string]int64{limits.RetainHumanActions: 1}})
	must(t, err)
	f.exec(fmt.Sprintf(`INSERT INTO settings (key, value, updated_at) VALUES ('%s', '{"backup_id":"bkp_1"}', 0)`, backup.SettingRestored))
	if _, err := f.svc.Run(f.ctx, admin); !errors.Is(err, ErrRestorePending) {
		t.Fatalf("run before acknowledgement: %v", err)
	}
	if next, err := f.svc.Next(f.ctx); err != nil || !next.IsZero() {
		t.Fatalf("next %v %v", next, err)
	}
	f.clk.Advance(48 * time.Hour)
	if n, err := f.svc.Due(f.ctx); err != nil || n != 0 {
		t.Fatalf("due %d %v", n, err)
	}
	st, err := f.svc.Status(f.ctx)
	must(t, err)
	if st.Blocked == "" || st.NextAt != nil {
		t.Fatalf("status %+v", st)
	}
	if f.count(`SELECT count(*) FROM human_actions WHERE id = 'hum_gone'`) != 1 {
		t.Fatal("deleted before the restore was acknowledged")
	}
	_, err = backup.AcknowledgeRestore(f.ctx, f.s, f.aw, changefeed.New(nil), admin)
	must(t, err)
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.Deleted[KindHumanActions] == 0 {
		t.Fatalf("after acknowledgement: %v", p.Deleted)
	}
}

// The scheduled pass is due FirstDelay after start, then a day after the last one started; it runs once.
func TestScheduledPassOnTheSharedTimer(t *testing.T) {
	f := newFx(t, 0)
	start := f.clk.Now()
	next, err := f.svc.Next(f.ctx)
	must(t, err)
	if !next.Equal(start.Add(10 * time.Minute)) {
		t.Fatalf("first next %v", next)
	}
	if n, _ := f.svc.Due(f.ctx); n != 0 {
		t.Fatal("ran before it was due")
	}
	f.clk.Advance(11 * time.Minute)
	n, err := f.svc.Due(f.ctx)
	must(t, err)
	if n != 1 {
		t.Fatal("scheduled pass did not start")
	}
	f.svc.Wait()
	next, err = f.svc.Next(f.ctx)
	must(t, err)
	if !next.Equal(start.Add(11*time.Minute + 24*time.Hour)) {
		t.Fatalf("next after a pass %v", next)
	}
	if n, _ := f.svc.Due(f.ctx); n != 0 {
		t.Fatal("ran twice")
	}
	st, err := f.svc.Status(f.ctx)
	must(t, err)
	if len(st.Passes) != 1 || st.Passes[0].Trigger != TriggerScheduled || st.Passes[0].State != StateSucceeded {
		t.Fatalf("passes %+v", st.Passes)
	}
	// A pass interrupted by a restart is marked so.
	f.exec(`INSERT INTO retention_passes (id, trigger, state, started_at) VALUES ('ret_x', 'manual', 'running', 1)`)
	must(t, f.svc.Recover(f.ctx))
	if f.count(`SELECT count(*) FROM retention_passes WHERE id = 'ret_x' AND state = 'interrupted'`) != 1 {
		t.Fatal("not interrupted")
	}
}

// Audit retention deletes the oldest events, keeps the head, records the pruned prefix, and the chain still
// verifies from the first retained event.
func TestAuditRetentionKeepsTheChainVerifiable(t *testing.T) {
	f := newFx(t, 30)
	appendN := func(n int) {
		for range n {
			must(t, f.s.W().TxNamed(f.ctx, "a", func(tx *store.Tx) error {
				_, err := f.aw.Append(f.ctx, tx, audit.Event{Actor: admin, Action: "test.event"})
				return err
			}))
		}
	}
	appendN(5)
	f.clk.Advance(40 * 24 * time.Hour)
	appendN(3)
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	// Only the 5 events from 40 days ago are older than 30 days; the pass's own events are new.
	if p.Deleted[KindAudit] != 5 {
		t.Fatalf("pruned %d", p.Deleted[KindAudit])
	}
	res, err := audit.Verify(f.ctx, f.s.R())
	must(t, err)
	if !res.OK || res.PrunedThrough != 5 || res.HeadSeq == 0 {
		t.Fatalf("verify %+v", res)
	}
	pr, err := audit.PrunedPrefix(f.ctx, f.s.R())
	must(t, err)
	if pr == nil || pr.Through != 5 || pr.Count != 5 {
		t.Fatalf("prefix %+v", pr)
	}
	if f.count(`SELECT count(*) FROM audit_events WHERE action = 'audit.pruned'`) != 1 {
		t.Fatal("no audit.pruned event")
	}
	if f.count(`SELECT count(*) FROM changes WHERE resource_type = 'audit' AND action = 'deleted'`) != 1 {
		t.Fatal("no change for the pruned audit events")
	}
	// Tampering after the prefix is still detected.
	f.exec(`UPDATE audit_events SET action = 'tampered' WHERE seq = 7`)
	res, err = audit.Verify(f.ctx, f.s.R())
	must(t, err)
	if res.OK || res.BrokenAt != 7 {
		t.Fatalf("tamper not detected: %+v", res)
	}
}

// Everything old is never pruned past the head: the chain keeps its newest event.
func TestAuditPruneKeepsTheHead(t *testing.T) {
	f := newFx(t, 30)
	for range 3 {
		must(t, f.s.W().TxNamed(f.ctx, "a", func(tx *store.Tx) error {
			_, err := f.aw.Append(f.ctx, tx, audit.Event{Actor: admin, Action: "test.event"})
			return err
		}))
	}
	f.clk.Advance(40 * 24 * time.Hour)
	var n, through int64
	must(t, f.s.W().TxNamed(f.ctx, "p", func(tx *store.Tx) error {
		var err error
		n, through, err = f.aw.Prune(f.ctx, tx, f.clk.Now().UnixMicro(), Actor)
		return err
	}))
	if n != 2 || through != 2 {
		t.Fatalf("pruned %d through %d", n, through)
	}
	res, err := audit.Verify(f.ctx, f.s.R())
	must(t, err)
	if !res.OK || res.Events != 2 || res.HeadSeq != 4 {
		t.Fatalf("verify %+v", res)
	}
}

func TestRunWhileRunningIsRefused(t *testing.T) {
	f := newFx(t, 0)
	f.svc.busy = true
	if _, err := f.svc.Run(f.ctx, admin); apperr.CodeOf(err) != apperr.CodeConflict {
		t.Fatalf("got %v", err)
	}
	if next, _ := f.svc.Next(f.ctx); !next.IsZero() {
		t.Fatal("next while busy")
	}
}
