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
		head := "bbbb"
		if h := f.heads[n]; h != "" {
			head = h
		}
		wts = append(wts, proto.WorktreeCheck{Path: "worktrees/" + n, Branch: "spaceman/" + n[len("spaceman-"):], Head: head})
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

// A run can have several tasks: the worktree is a candidate only if every one is done or cancelled. A failed or an open
// task next to a done one keeps it, whichever the list keeps.
func TestPruneSpaceEveryTaskOfTheRunMustBeFinished(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	names := map[string]string{}
	for i, other := range []string{"failed", "review", "running"} {
		n := 20 + i
		f.run(fmt.Sprintf("run_%d", n), fmt.Sprintf("spaceman/tsk_%d", n), "exited", f.clk.Now().Add(-20*24*time.Hour))
		// The finished task has the higher rank (so it comes last in the list, the case that used to win); the other the lower.
		f.taskAt(fmt.Sprintf("tsk_%da", n), other, fmt.Sprintf("run_%d", n), 1, "NULL")
		f.taskAt(fmt.Sprintf("tsk_%db", n), "done", fmt.Sprintf("run_%d", n), 2, `(SELECT ended_at FROM runs WHERE id = ?)`, fmt.Sprintf("run_%d", n))
		names[other] = fmt.Sprintf("spaceman-tsk_%d", n)
	}
	// Two finished tasks of one run: eligible.
	f.run("run_30", "spaceman/tsk_30", "exited", f.clk.Now().Add(-20*24*time.Hour))
	f.taskAt("tsk_30a", "done", "run_30", 1, `(SELECT ended_at FROM runs WHERE id = 'run_30')`)
	f.taskAt("tsk_30b", "cancelled", "run_30", 2, `(SELECT ended_at FROM runs WHERE id = 'run_30')`)
	f.reportWith(names["failed"], names["review"], names["running"], "spaceman-tsk_30")
	if _, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10); err != nil {
		t.Fatal(err)
	}
	got := queuedNames(f)
	for k, n := range names {
		if got[n] {
			t.Errorf("the worktree of the run with a %s task was asked for removal", k)
		}
	}
	if !got["spaceman-tsk_30"] || !got["spaceman-tsk_1"] || len(got) != 2 {
		t.Fatalf("queued %v", got)
	}
	l, err := f.svc.List(f.ctx, "spc_a", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Worktrees {
		if e.Name == names["failed"] && (e.Prunable || e.Task.Status != "failed") {
			t.Errorf("listing: %+v %+v", e, e.Task)
		}
	}
}

// The age counts from the later of the run's end and the latest finished_at of its tasks; a finished task without
// finished_at is never eligible; candidates are ordered by that time.
func TestPruneSpaceTimeBase(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	ago := func(d int) string {
		return fmt.Sprintf("%d", f.clk.Now().Add(-time.Duration(d)*24*time.Hour).UnixMicro())
	}
	mk := func(n, endedDays int, tasks ...[2]string) string {
		id := fmt.Sprintf("%d", n)
		f.run("run_"+id, "spaceman/tsk_"+id, "exited", f.clk.Now().Add(-time.Duration(endedDays)*24*time.Hour))
		for i, tk := range tasks {
			fin := "NULL"
			if tk[1] != "" {
				fin = tk[1]
			}
			f.taskAt(fmt.Sprintf("tsk_%d_%d", n, i), tk[0], "run_"+id, i+1, fin)
		}
		return "spaceman-tsk_" + id
	}
	lateDone := mk(40, 20, [2]string{"done", ago(1)})                                // run ended long ago, marked done yesterday: too new
	laterRun := mk(41, 1, [2]string{"done", ago(20)})                                // marked done long ago, run ended yesterday: too new
	noFinish := mk(42, 20, [2]string{"done", ""})                                    // done without finished_at
	oneMissing := mk(43, 20, [2]string{"done", ago(20)}, [2]string{"cancelled", ""}) // one of two lacks it
	late2 := mk(44, 20, [2]string{"done", ago(20)}, [2]string{"done", ago(2)})       // the latest of the tasks counts: too new
	old := mk(45, 12, [2]string{"done", ago(30)})                                    // 12 days from the run's end
	older := mk(46, 30, [2]string{"done", ago(25)})                                  // 25 days from the task: the oldest
	mid := mk(47, 13, [2]string{"done", ago(13)})                                    // 13 days
	late := mk(48, 15, [2]string{"done", ago(8)})                                    // run ended 15 days ago but the task 8: counts as 8, the newest
	f.reportWith(lateDone, laterRun, noFinish, oneMissing, late2, old, older, mid, late)
	c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range c {
		got = append(got, e.Name)
	}
	// Order is by the later time, oldest first (not by the run's end: late ended before mid and old).
	if want := []string{older, mid, old, "spaceman-tsk_1", late}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("candidates %v, want %v", got, want)
	}
}

// pruneAndAnswer runs a pass and answers the newest queued removal of name.
func pruneAndAnswer(t *testing.T, f *fx, name, result, code, head string) {
	t.Helper()
	if _, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10); err != nil {
		t.Fatal(err)
	}
	ids, pl := f.commands()
	cmd := ""
	for i, id := range ids {
		if pl[i].Name == name {
			cmd = id
		}
	}
	if cmd == "" {
		t.Fatalf("no removal of %s queued", name)
	}
	f.answerHead(cmd, name, result, code, "why", head)
}

// Only the latest answer counts: a refusal that a later (failed) attempt followed does not push the worktree back.
func TestPruneSpaceLatestAnswerDecidesRefusedLast(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	stuck := f.addFinished(0, "done", 30)
	f.reportWith(stuck)
	pruneAndAnswer(t, f, stuck, proto.WorktreeRefused, "dirty", "bbbb")
	if c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 1); err != nil || len(c) != 1 || c[0].Name != "spaceman-tsk_1" {
		t.Fatalf("after the refusal: %v %+v", err, c)
	}
	pruneAndAnswer(t, f, stuck, proto.WorktreeFailed, "", "bbbb")
	if c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 1); err != nil || len(c) != 1 || c[0].Name != stuck {
		t.Fatalf("after a later answer that was not a refusal: %v %+v", err, c)
	}
}

// The refusals the node makes before it reads the HEAD (dirty, untracked, locked, ...) carry none: the HEAD the
// listing showed when the removal was requested stands in, so an always-dirty worktree is tried last too.
func TestPruneSpaceRefusalWithoutHeadIsTriedLast(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	stuck := f.addFinished(0, "done", 30)
	f.reportWith(stuck)
	pruneAndAnswer(t, f, stuck, proto.WorktreeRefused, "dirty", "")
	if c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 1); err != nil || len(c) != 1 || c[0].Name != "spaceman-tsk_1" {
		t.Fatalf("after a refusal with no HEAD: %v %+v", err, c)
	}
	f.heads[stuck] = "eeee"
	f.reportWith(stuck)
	if c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 1); err != nil || len(c) != 1 || c[0].Name != stuck {
		t.Fatalf("after a new HEAD: %v %+v", err, c)
	}
}

// A worktree whose last removal was refused at its current HEAD is tried after the ones never refused, so refusals
// that repeat cannot use up the batch; a new HEAD makes it a fresh candidate again.
func TestPruneSpaceTriesRefusedLast(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 20 * time.Millisecond
	stuck := f.addFinished(0, "done", 30) // older than tsk_1 (10 days): first in line until it is refused
	f.reportWith(stuck)
	if _, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10); err != nil {
		t.Fatal(err)
	}
	ids, pl := f.commands()
	for i, id := range ids {
		if pl[i].Name == stuck {
			f.answer(id, stuck, proto.WorktreeRefused, "dirty", "changed files") // head bbbb
		}
	}
	c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 1)
	if err != nil || len(c) != 1 || c[0].Name != "spaceman-tsk_1" {
		t.Fatalf("after a refusal at the same HEAD: %v %+v", err, c)
	}
	f.heads[stuck] = "eeee" // the worktree moved on
	f.reportWith(stuck)
	c, err = f.svc.Candidates(f.ctx, "spc_a", "", 7, 1)
	if err != nil || len(c) != 1 || c[0].Name != stuck {
		t.Fatalf("after a new HEAD: %v %+v", err, c)
	}
}

// With more than 1000 runs in the space the owner of an old worktree is not found: it shows as unowned and is never
// a candidate (fails safe).
func TestPruneSpaceKeepsWorktreesBeyondTheRunWindow(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	hour := f.clk.Now().Add(-time.Hour).UnixMicro()
	for part := 0; part < 5; part++ { // in parts: one big transaction would hold the write lock too long
		f.exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 200)
			INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, ended_at, updated_at)
			SELECT 'run_bulk' || (? + i), 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_x', 'running', 'exited', '{}', ?, 't', ?, ? FROM n`,
			part*200, hour, hour, hour)
	}
	c, err := f.svc.Candidates(f.ctx, "spc_a", "", 7, 0)
	if err != nil || len(c) != 0 {
		t.Fatalf("%v %+v", err, c)
	}
	l, err := f.svc.List(f.ctx, "spc_a", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Worktrees {
		if e.Name == "spaceman-tsk_1" && (e.Owned || e.Prunable) {
			t.Fatalf("%+v", e)
		}
	}
}

// A live run is never a candidate even when ended_at is set (a run reported live keeps no end, but the check must not
// depend on that).
func TestPrunableNeverLive(t *testing.T) {
	end := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e := Entry{Owned: true, Run: &RunRef{EndedAt: &end}, Task: &TaskRef{Status: "done"}, tasksDone: true, finishedAt: &end}
	now := end.Add(30 * 24 * time.Hour)
	if !prunable(e, 7, now) {
		t.Fatal("the control entry is not prunable")
	}
	e.Run.Live = true
	if prunable(e, 7, now) {
		t.Fatal("a live run was prunable")
	}
}

// The answer wait is one deadline for the whole space, on the wall clock: with a frozen injected clock, three
// unanswered removals wait about PruneWait together, not each the full time.
func TestPruneSpaceWaitIsOneDeadline(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.PruneWait = 300 * time.Millisecond
	a, b := f.addFinished(3, "done", 20), f.addFinished(4, "done", 20)
	f.reportWith(a, b)
	start := time.Now()
	sum, err := f.svc.PruneSpace(f.ctx, retentionActor, "spc_a", 7, 10)
	if err != nil || sum.Pending != 3 {
		t.Fatalf("%v %+v", err, sum)
	}
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Fatalf("waited %v for 3 answers with a 300ms deadline", d)
	}
}
