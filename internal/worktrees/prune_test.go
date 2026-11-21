package worktrees

import (
	"fmt"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

var retentionActor = audit.Actor{Type: audit.ActorSystem, ID: "retention"}

// addFinished adds a finished run and task (status) with a worktree to the report on top of the fixture's worktrees.
func (f *fx) addFinished(n int, status string, endedDaysAgo int) string {
	f.t.Helper()
	id := fmt.Sprintf("%d", n)
	f.run("run_"+id, "spaceman/tsk_"+id, "exited", f.clk.Now().Add(-time.Duration(endedDaysAgo)*24*time.Hour))
	f.task("tsk_"+id, status, "run_"+id)
	return "spaceman-tsk_" + id
}

func (f *fx) reportWith(names ...string) {
	f.t.Helper()
	at := f.clk.Now().Add(-time.Minute)
	wts := []proto.WorktreeCheck{{Path: "worktrees/spaceman-tsk_1", Branch: "spaceman/tsk_1", Head: "bbbb"},
		{Path: "worktrees/spaceman-tsk_2", Branch: "spaceman/tsk_2", Head: "cccc"}, {Path: "worktrees/hand", Branch: "hand", Head: "dddd"}}
	for _, n := range names {
		wts = append(wts, proto.WorktreeCheck{Path: "worktrees/" + n, Branch: "spaceman/" + n[len("spaceman-"):], Head: "bbbb"})
	}
	f.rp.OnStatus(f.ctx, "nod_1", proto.RepoStatusReport{CommandID: "cmd_p", SpaceID: "spc_a", CheckedAt: at, Repositories: []proto.RepoState{
		{ID: f.repo.ID, Path: "api", Present: true, LastFetchAt: &at, Check: &proto.RepoCheck{Path: "api", Branch: "main", Head: "aaaa", Worktrees: wts}}}})
}

func queuedNames(f *fx) map[string]bool {
	_, pl := f.commands()
	m := map[string]bool{}
	for _, p := range pl {
		m[p.Name] = true
	}
	return m
}

// The retention rule asks for the removal of finished work only: a done task (tsk_1, 10 days) and a cancelled one are
// pruned; a failed task, an open task with a live run, an unowned worktree and a run that ended too recently are kept.
func TestPruneSpacePicksOnlyEligible(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 50 * time.Millisecond
	cancelled := f.addFinished(3, "cancelled", 20)
	failed := f.addFinished(4, "failed", 20)
	recent := f.addFinished(5, "done", 2)
	f.reportWith(cancelled, failed, recent)

	sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Candidates != 2 || sum.Requested != 2 || sum.Pending != 2 || sum.Skipped != "" {
		t.Fatalf("summary %+v", sum)
	}
	got := queuedNames(f)
	if len(got) != 2 || !got["spaceman-tsk_1"] || !got[cancelled] {
		t.Fatalf("queued %v", got)
	}
	for _, n := range []string{failed, recent, "spaceman-tsk_2", "hand"} {
		if got[n] {
			t.Errorf("%s was asked for removal", n)
		}
	}
	var n int
	if err := f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = ? AND actor_id = 'retention'`, ActionRequested).Scan(&n); err != nil || n != 2 {
		t.Fatalf("%d audit events of the requests", n)
	}
}

// A live run on the branch of an otherwise eligible worktree: the service refuses it, nothing is queued for it.
func TestPruneSpaceKeepsLiveRunBranch(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	// A newer live run on tsk_1's branch holds the worktree (the owner is the newest run, which is live: not eligible).
	f.run("run_again", "spaceman/tsk_1", "running", time.Time{})
	f.exec(`UPDATE runs SET created_at = created_at + 1000000 WHERE id = 'run_again'`)
	sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10)
	if err != nil || sum.Candidates != 0 || sum.Requested != 0 {
		t.Fatalf("%v %+v", err, sum)
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatalf("queued %v", ids)
	}
}

// The batch bound: of five eligible worktrees at most limit are asked, oldest first; Candidates counts all.
func TestPruneSpaceBatchBound(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	var names []string
	for i, d := range []int{40, 30, 25, 20} {
		names = append(names, f.addFinished(10+i, "done", d))
	}
	f.reportWith(names...) // five eligible with tsk_1 (10 days)
	sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 3)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Candidates != 5 || sum.Requested != 3 {
		t.Fatalf("summary %+v", sum)
	}
	got := queuedNames(f)
	if len(got) != 3 || !got[names[0]] || !got[names[1]] || !got[names[2]] {
		t.Fatalf("oldest three not picked: %v", got)
	}
}

// 0 days or a 0 limit is off.
func TestPruneSpaceOff(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	for _, c := range []struct {
		days  int64
		limit int
	}{{0, 10}, {7, 0}, {-1, 10}} {
		sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", c.days, c.limit)
		if err != nil || sum != (PruneSummary{}) {
			t.Fatalf("%+v: %v %+v", c, err, sum)
		}
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatalf("queued %v", ids)
	}
}

// A node below protocol 16 is skipped without an error and nothing is queued.
func TestPruneSpaceSkipsOldNode(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove-1)
	sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10)
	if err != nil || sum.Skipped == "" || sum.Requested != 0 || sum.Candidates != 1 {
		t.Fatalf("%v %+v", err, sum)
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatalf("queued %v", ids)
	}
}

// A node without a valid lease (not connected) is skipped without an error, like an old node.
func TestPruneSpaceSkipsDisconnectedNode(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.exec(`UPDATE execution_leases SET expires_at = 0 WHERE space_id = 'spc_a'`)
	sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10)
	if err != nil || sum.Skipped == "" || sum.Requested != 0 || sum.Failed != 0 {
		t.Fatalf("%v %+v", err, sum)
	}
}

// The node's answers are counted: removed and refused (a refusal keeps the worktree).
func TestPruneSpaceCountsAnswers(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 5 * time.Second
	other := f.addFinished(3, "done", 20)
	f.reportWith(other)
	done := make(chan PruneSummary, 1)
	go func() {
		sum, _ := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10)
		done <- sum
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		ids, pl := f.commands()
		if len(ids) == 2 {
			for i, id := range ids {
				if pl[i].Name == other {
					f.answer(id, pl[i].Name, proto.WorktreeRefused, "dirty", "changed files")
				} else {
					f.answer(id, pl[i].Name, proto.WorktreeRemovedOK, "", "")
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("removals not queued")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sum := <-done; sum.Removed != 1 || sum.Refused != 1 || sum.Requested != 2 {
		t.Fatalf("summary %+v", sum)
	}
}
