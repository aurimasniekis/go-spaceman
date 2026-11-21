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
	"github.com/aurimasniekis/go-spaceman/internal/worktrees"
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

// The bot's message mappings older than 30 days are pruned by the daily pass, in batches; newer ones stay.
func TestPassPrunesTelegramMessages(t *testing.T) {
	f := newFx(t, 0)
	now := f.clk.Now()
	ins := func(chat, msg int64, age time.Duration) string {
		return fmt.Sprintf(`INSERT INTO telegram_messages (chat_id, message_id, kind, created_at) VALUES (%d, %d, 'card', %d)`, chat, msg, now.Add(-age).UnixMicro())
	}
	var qs []string
	for i := int64(1); i <= telegramPruneBatch+50; i++ { // more than one batch
		qs = append(qs, ins(1, i, 31*24*time.Hour))
		if len(qs) == 50 { // short seed transactions
			f.exec(qs...)
			qs = nil
		}
	}
	qs = append(qs, ins(2, 1, 29*24*time.Hour), ins(2, 2, time.Hour))
	f.exec(qs...)
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if want := int64(telegramPruneBatch + 50); p.Deleted[KindTelegram] != want {
		t.Fatalf("pruned %d, want %d", p.Deleted[KindTelegram], want)
	}
	if n := f.count(`SELECT count(*) FROM telegram_messages`); n != 2 {
		t.Fatalf("%d mappings left, want the 2 newer ones", n)
	}
}

// A one-time notice row (ADR-0089) is never pruned: it is what keeps the notice from being sent again.
func TestPassKeepsTelegramNotices(t *testing.T) {
	f := newFx(t, 0)
	old := f.clk.Now().Add(-40 * 24 * time.Hour).UnixMicro()
	f.exec(fmt.Sprintf(`INSERT INTO telegram_messages (chat_id, message_id, kind, resource_type, created_at) VALUES (1, 1, 'notice', 'threads', %d)`, old),
		fmt.Sprintf(`INSERT INTO telegram_messages (chat_id, message_id, kind, created_at) VALUES (1, 2, 'card', %d)`, old))
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.Deleted[KindTelegram] != 1 {
		t.Fatalf("pruned %d, want the card only", p.Deleted[KindTelegram])
	}
	if n := f.count(`SELECT count(*) FROM telegram_messages WHERE kind = 'notice'`); n != 1 {
		t.Fatal("the notice row was pruned")
	}
}

// The 30-day boundary: a mapping an hour past it is pruned, one an hour inside it stays.
func TestPassPrunesTelegramMessagesAtTheBoundary(t *testing.T) {
	f := newFx(t, 0)
	now := f.clk.Now()
	day := 24 * time.Hour
	ins := func(msg int64, age time.Duration) string {
		return fmt.Sprintf(`INSERT INTO telegram_messages (chat_id, message_id, kind, created_at) VALUES (1, %d, 'card', %d)`, msg, now.Add(-age).UnixMicro())
	}
	f.exec(ins(1, 30*day+time.Hour), ins(2, 30*day-time.Hour))
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if p.Deleted[KindTelegram] != 1 {
		t.Fatalf("pruned %d, want only the one past 30 days", p.Deleted[KindTelegram])
	}
	if n := f.count(`SELECT count(*) FROM telegram_messages WHERE message_id = 2`); n != 1 {
		t.Fatal("the mapping an hour inside the window was pruned")
	}
}

type pruneCall struct {
	space string
	days  int64
	limit int
}

// withPrune installs a fake worktree pruner that records its calls and reports every asked removal as removed (up to
// the candidates it is given).
func (f *fx) withPrune(candidates int) *[]pruneCall {
	calls := &[]pruneCall{}
	f.svc.o.PruneWorktrees = func(_ context.Context, actor audit.Actor, space string, days int64, limit int) (worktrees.PruneSummary, error) {
		if actor != Actor {
			f.t.Errorf("actor %+v", actor)
		}
		*calls = append(*calls, pruneCall{space, days, limit})
		n := min(candidates, limit)
		return worktrees.PruneSummary{Candidates: candidates, Requested: n, Removed: n}, nil
	}
	return calls
}

// The worktree step runs only for a space whose worktree_retention_days is above 0 (the default 0 is off), with the
// space's days and the per-space bound, and its removals are counted.
func TestPassPrunesWorktreesOnlyWhenEnabled(t *testing.T) {
	f := newFx(t, 0)
	calls := f.withPrune(3)
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	if len(*calls) != 0 || p.Deleted[KindWorktrees] != 0 {
		t.Fatalf("pruned with the limit off: %v %v", *calls, p.Deleted)
	}
	_, err = f.lim.Set(f.ctx, admin, "spc_a", 0, limits.Input{Values: map[string]int64{limits.WorktreeRetention: 14}})
	must(t, err)
	p, err = f.svc.Run(f.ctx, admin)
	must(t, err)
	if len(*calls) != 1 || (*calls)[0] != (pruneCall{"spc_a", 14, WorktreeBatchPerSpace}) || p.Deleted[KindWorktrees] != 3 {
		t.Fatalf("calls %v deleted %v", *calls, p.Deleted)
	}
	// Setting it back to 0 turns it off.
	_, err = f.lim.Set(f.ctx, admin, "spc_a", 1, limits.Input{Values: map[string]int64{limits.WorktreeRetention: 0}})
	must(t, err)
	if _, err = f.svc.Run(f.ctx, admin); err != nil || len(*calls) != 1 {
		t.Fatalf("pruned after turning it off: %v %v", err, *calls)
	}
}

// The bounds: a space is asked for at most WorktreeBatchPerSpace, and the pass for at most WorktreeBatchPerPass; later
// spaces get what is left and none once it is spent.
func TestPassBoundsWorktreePrune(t *testing.T) {
	f := newFx(t, 0)
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("spc_x%d", i)
		f.exec(fmt.Sprintf(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('%s', 'x%d', 'X', 'nod_1', 'active', 'x%d', 0, 't', 0)`, id, i, i))
	}
	for _, id := range []string{"spc_a", "spc_b", "spc_x0", "spc_x1", "spc_x2", "spc_x3", "spc_x4", "spc_x5"} {
		_, err := f.lim.Set(f.ctx, admin, id, 0, limits.Input{Values: map[string]int64{limits.WorktreeRetention: 7}})
		must(t, err)
	}
	calls := f.withPrune(1000)
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	total := 0
	for _, c := range *calls {
		if c.limit > WorktreeBatchPerSpace || c.limit <= 0 {
			t.Fatalf("per-space bound broken: %v", *calls)
		}
		total += c.limit
	}
	if total != WorktreeBatchPerPass || p.Deleted[KindWorktrees] != WorktreeBatchPerPass {
		t.Fatalf("asked %d, removed %d, want %d: %v", total, p.Deleted[KindWorktrees], WorktreeBatchPerPass, *calls)
	}
	if len(*calls) != WorktreeBatchPerPass/WorktreeBatchPerSpace {
		t.Fatalf("spaces asked: %d", len(*calls))
	}
}

// A pruner error fails the pass for that space only; the others still run.
func TestPassWorktreePruneErrorContinues(t *testing.T) {
	f := newFx(t, 0)
	for _, id := range []string{"spc_a", "spc_b"} {
		_, err := f.lim.Set(f.ctx, admin, id, 0, limits.Input{Values: map[string]int64{limits.WorktreeRetention: 7}})
		must(t, err)
	}
	var seen []string
	f.svc.o.PruneWorktrees = func(_ context.Context, _ audit.Actor, space string, _ int64, _ int) (worktrees.PruneSummary, error) {
		seen = append(seen, space)
		return worktrees.PruneSummary{}, errors.New("boom")
	}
	p, err := f.svc.Run(f.ctx, admin)
	if err == nil || p.State != StateFailed || len(seen) != 2 {
		t.Fatalf("%v %+v %v", err, p, seen)
	}
}

// enableWorktrees turns the worktree retention on for the fixture's spaces and the extra ones.
func (f *fx) enableWorktrees(extra int) {
	f.t.Helper()
	ids := []string{"spc_a", "spc_b"}
	for i := 0; i < extra; i++ {
		id := fmt.Sprintf("spc_x%d", i)
		f.exec(fmt.Sprintf(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('%s', 'x%d', 'X', 'nod_1', 'active', 'x%d', 0, 't', 0)`, id, i, i))
		ids = append(ids, id)
	}
	for _, id := range ids {
		_, err := f.lim.Set(f.ctx, admin, id, 0, limits.Input{Values: map[string]int64{limits.WorktreeRetention: 7}})
		must(f.t, err)
	}
}

// The pass's own cap, when it is not a multiple of the per-space bound: the last space is asked for what is left, not
// for the whole per-space bound.
func TestPassWorktreeCapIsNotAMultipleOfTheSpaceBound(t *testing.T) {
	f := newFx(t, 0)
	f.enableWorktrees(3)
	f.svc.o.WorktreeBatchPerPass = 25
	calls := f.withPrune(1000)
	p, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	var limits []int
	for _, c := range *calls {
		limits = append(limits, c.limit)
	}
	if fmt.Sprint(limits) != "[10 10 5]" || p.Deleted[KindWorktrees] != 25 {
		t.Fatalf("limits %v removed %d", limits, p.Deleted[KindWorktrees])
	}
}

// Each pass starts at the space after the one where the last pass spent the whole batch, so spaces late in the list
// are not starved by the early ones.
func TestPassWorktreePruneRotatesTheStartSpace(t *testing.T) {
	f := newFx(t, 0)
	f.enableWorktrees(6) // 8 spaces; a pass serves 5
	calls := f.withPrune(1000)
	_, err := f.svc.Run(f.ctx, admin)
	must(t, err)
	first := append([]pruneCall(nil), *calls...)
	*calls = nil
	_, err = f.svc.Run(f.ctx, admin)
	must(t, err)
	if len(first) != 5 || len(*calls) != 5 {
		t.Fatalf("served %d then %d spaces", len(first), len(*calls))
	}
	if (*calls)[0].space == first[0].space {
		t.Fatalf("the second pass started at the same space: %v then %v", first, *calls)
	}
	served := map[string]bool{}
	for _, c := range append(first, *calls...) {
		served[c.space] = true
	}
	if len(served) != 8 {
		t.Fatalf("two passes served %d of 8 spaces", len(served))
	}
}
