package worktrees

import (
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/messages"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

func (f *fx) withMessages() *messages.Service {
	f.t.Helper()
	m := messages.New(f.s, f.aw, changefeed.New(nil))
	f.svc.Control = m
	return m
}

type sentMessage struct{ from, to, kind, subject, body, dedup string }

func (f *fx) sent() []sentMessage {
	f.t.Helper()
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT from_kind, to_kind, kind, subject, body, dedup_key FROM messages WHERE space_id = 'spc_a' ORDER BY created_at, id`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []sentMessage
	for rows.Next() {
		var m sentMessage
		if err := rows.Scan(&m.from, &m.to, &m.kind, &m.subject, &m.body, &m.dedup); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return out
}

// The cleanup message goes to control once per period: a second call, another central on the same store and a
// restart (a new service) all find the period's key; the next period sends again. The fixture has one finished
// worktree (10 days), one with a live run and one without an owner.
func TestNotifyCleanupSendsOncePerPeriod(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	m := f.withMessages()
	for i := 0; i < 2; i++ {
		if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent != (i == 0) {
			t.Fatalf("call %d: sent %v, err %v", i, sent, err)
		}
	}
	other := New(f.s, f.aw, f.svc.fleet, f.rp, f.svc.queue, f.clk, nil)
	other.Control = m
	if sent, err := other.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("second central: sent %v, err %v", sent, err)
	}
	if got := f.sent(); len(got) != 1 || got[0].dedup != cleanupKey(f.clk.Now(), 7) {
		t.Fatalf("messages after the first period: %+v", got)
	}
	// Into the next period (at most 7 days on).
	first := cleanupKey(f.clk.Now(), 7)
	days := 0
	for ; cleanupKey(f.clk.Now(), 7) == first; days++ {
		if days > 7 {
			t.Fatal("the period never changed")
		}
		if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
			t.Fatalf("day %d of the period: sent %v, err %v", days, sent, err)
		}
		f.clk.Advance(24 * time.Hour)
	}
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || !sent {
		t.Fatalf("next period: sent %v, err %v", sent, err)
	}
	got := f.sent()
	if len(got) != 2 || got[0].dedup == got[1].dedup {
		t.Fatalf("messages: %+v", got)
	}
}

// Nothing is sent when the period is 0 (off), when there is no sender, or when nothing is finished; and the check is
// retried later: when work finishes the message goes out in the same period.
func TestNotifyCleanupSkips(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("without a sender: %v %v", sent, err)
	}
	f.withMessages()
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 0); err != nil || sent {
		t.Fatalf("period 0: %v %v", sent, err)
	}
	f.exec(`UPDATE tasks SET status = 'running', finished_at = NULL WHERE id = 'tsk_1'`) // not finished any more
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("nothing finished: %v %v", sent, err)
	}
	if n := len(f.sent()); n != 0 {
		t.Fatalf("%d messages", n)
	}
	f.exec(`UPDATE tasks SET status = 'done', finished_at = (SELECT ended_at FROM runs WHERE id = 'run_done') WHERE id = 'tsk_1'`)
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || !sent {
		t.Fatalf("after it finished: %v %v", sent, err)
	}
}

// A paused or archived space is not asked; it is asked once it is active again, in the same period. A space whose report
// is from an earlier node (after a relocation) says nothing about worktrees that may be gone.
func TestNotifyCleanupOnlyActiveSpacesWithCurrentReport(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.withMessages()
	for _, state := range []string{"paused", "archived"} {
		f.exec(`UPDATE spaces SET state = ? WHERE id = 'spc_a'`, state)
		if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
			t.Fatalf("%s: %v %v", state, sent, err)
		}
	}
	f.exec(`UPDATE spaces SET state = 'active' WHERE id = 'spc_a'`)
	f.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_2', 'n2', 'active', ?, 0, 't', 0)`, proto.MinorWorktreeRemove)
	f.exec(`UPDATE spaces SET node_id = 'nod_2' WHERE id = 'spc_a'`)
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("stale report: %v %v", sent, err)
	}
	f.exec(`UPDATE spaces SET node_id = 'nod_1' WHERE id = 'spc_a'`)
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || !sent {
		t.Fatalf("active with a current report: %v %v", sent, err)
	}
	if sent, err := f.svc.NotifyCleanup(f.ctx, "no_such_space", 7); err != nil || sent {
		t.Fatalf("unknown space: %v %v", sent, err)
	}
}

// A space with no repository report has nothing to tidy.
func TestNotifyCleanupNoReport(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.withMessages()
	f.exec(`DELETE FROM runs`)
	f.exec(`DELETE FROM tasks`)
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("no owned worktree: %v %v", sent, err)
	}
	f.exec(`DELETE FROM space_repositories`)
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("no repository: %v %v", sent, err)
	}
}

// The message is a system notification to control that says what was counted and which tools to use, and it
// carries no worktree names, branches or paths (they are agent-writable).
func TestNotifyCleanupContent(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.withMessages()
	if sum, err := f.svc.CleanupSummary(f.ctx, "spc_a"); err != nil || sum != (CleanupSummary{Total: 3, Finished: 1, Unowned: 1, OldestDays: 10}) {
		t.Fatalf("summary %+v, err %v", sum, err)
	}
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 14); err != nil || !sent {
		t.Fatalf("%v %v", sent, err)
	}
	got := f.sent()
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	m := got[0]
	if m.from != "system" || m.to != "control" || m.kind != "notification" || m.subject != CleanupMessageSubject || !strings.HasPrefix(m.dedup, "worktree-cleanup:14d:") {
		t.Fatalf("message %+v", m)
	}
	for _, want := range []string{"3 worktrees", "1 of them finished", "oldest for 10 days", "1 were not created by a SpaceMan run", "once per 14-day period",
		"spaceman_worktrees_list", "spaceman_worktrees_prune", "spaceman_worktree_remove", "Nothing has been removed"} {
		if !strings.Contains(m.body, want) {
			t.Errorf("body lacks %q:\n%s", want, m.body)
		}
	}
	for _, leak := range []string{"tsk_1", "spaceman-tsk", "hand", "api"} {
		if strings.Contains(m.body, leak) {
			t.Errorf("body names %q", leak)
		}
	}
}

// A finished worktree whose last removal was refused at its current HEAD is not counted: control is not asked to tidy
// what the node will refuse again. With nothing else finished there is no message; a later answer that is no refusal
// brings it back.
func TestNotifyCleanupLeavesOutRefusedWorktrees(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	f.withMessages()
	pruneAndAnswer(t, f, "spaceman-tsk_1", proto.WorktreeRefused, "dirty", "")
	if sum, err := f.svc.CleanupSummary(f.ctx, "spc_a"); err != nil || sum.Finished != 0 || sum.Total != 3 {
		t.Fatalf("summary %+v, err %v", sum, err)
	}
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || sent {
		t.Fatalf("only a refused worktree is finished: %v %v", sent, err)
	}
	pruneAndAnswer(t, f, "spaceman-tsk_1", proto.WorktreeFailed, "", "bbbb")
	if sent, err := f.svc.NotifyCleanup(f.ctx, "spc_a", 7); err != nil || !sent {
		t.Fatalf("after a later answer that was not a refusal: %v %v", sent, err)
	}
}

// The period's key is the number of whole periods since the epoch.
func TestCleanupKey(t *testing.T) {
	d := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	if a, b := cleanupKey(d, 7), cleanupKey(d.Add(time.Hour), 7); a != b {
		t.Fatalf("%s %s", a, b)
	}
	if cleanupKey(d, 7) == cleanupKey(d, 14) {
		t.Fatal("period must be part of the key")
	}
	if a, b := cleanupKey(d, 1), cleanupKey(d.Add(24*time.Hour), 1); a == b {
		t.Fatalf("daily keys equal: %s", a)
	}
}
