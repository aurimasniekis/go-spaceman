package worktrees

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/agenttools"
	"github.com/aurimasniekis/go-spaceman/internal/apperr"
	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/changefeed"
	"github.com/aurimasniekis/go-spaceman/internal/clock"
	"github.com/aurimasniekis/go-spaceman/internal/eventbus"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	"github.com/aurimasniekis/go-spaceman/internal/keyring/keyringtest"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/repos"
	"github.com/aurimasniekis/go-spaceman/internal/store"
	"github.com/aurimasniekis/go-spaceman/internal/store/storetest"
)

var admin = audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}

type fx struct {
	t    *testing.T
	ctx  context.Context
	s    *store.Store
	clk  *clock.Fake
	rp   *repos.Service
	svc  *Service
	reg  *agenttools.Registry
	repo *repos.Repository
}

// newFx seeds a node (protocol minor minor) with spc_a holding a valid lease, a configured repository "api" and a
// repository report listing worktrees/spaceman-tsk_1 (finished work), worktrees/spaceman-tsk_2 (a live run), worktrees/hand
// (no run) and a worktree outside worktrees/.
func newFx(t *testing.T, minor int) *fx {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC))
	s := storetest.Open(t, storetest.WithClock(clk))
	kr := keyringtest.Load(t, s)
	aw, ch := audit.NewWriter(kr), changefeed.New(nil)
	fl := fleet.New(s, kr, aw, eventbus.New(), ch, fleet.Options{Clock: clk})
	rp := repos.New(s, aw, ch, fl, repos.Options{Clock: clk})
	q := queue.New(s, aw, ch)
	f := &fx{t: t, ctx: context.Background(), s: s, clk: clk, rp: rp, svc: New(s, aw, fl, rp, q, clk, nil)}
	f.svc.Wait = 2 * time.Second
	f.reg = agenttools.New(agenttools.Deps{Extra: []func(*agenttools.Registry){Tools(f.svc)}})
	exp := clk.Now().Add(time.Hour).UnixMicro()
	f.exec(`INSERT INTO nodes (id, name, state, protocol_minor, created_at, created_by, updated_at) VALUES ('nod_1', 'n1', 'active', ?, 0, 't', 0)`, minor)
	f.exec(`INSERT INTO spaces (id, slug, name, node_id, state, workdir, created_at, created_by, updated_at) VALUES ('spc_a', 'aaa', 'A', 'nod_1', 'active', 'a', 0, 't', 0)`)
	f.exec(`INSERT INTO execution_leases (space_id, node_id, generation, state, granted_at, renewed_at, expires_at) VALUES ('spc_a', 'nod_1', 1, 'active', 0, 0, ?)`, exp)
	r, err := rp.Create(f.ctx, admin, "spc_a", repos.Input{Name: "api", URL: "https://git.example.com/api.git"})
	if err != nil {
		t.Fatal(err)
	}
	f.repo = r
	f.run("run_done", "spaceman/tsk_1", "exited", clk.Now().Add(-10*24*time.Hour))
	f.run("run_live", "spaceman/tsk_2", "running", time.Time{})
	f.task("tsk_1", "done", "run_done")
	f.task("tsk_2", "running", "run_live")
	f.report()
	return f
}

func (f *fx) exec(q string, args ...any) {
	f.t.Helper()
	if err := f.s.W().TxNamed(f.ctx, "seed", func(tx *store.Tx) error {
		_, err := tx.SQL.ExecContext(f.ctx, q, args...)
		return err
	}); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *fx) run(id, branch, observed string, ended time.Time) {
	f.t.Helper()
	launch, _ := json.Marshal(fleet.LaunchSpec{Worktree: &proto.Worktree{Repo: "api", Branch: branch}})
	var end any
	if !ended.IsZero() {
		end = ended.UnixMicro()
	}
	created := f.clk.Now().Add(-11 * 24 * time.Hour).UnixMicro()
	f.exec(`INSERT INTO runs (id, space_id, node_id, generation, kind, role, harness, instance_id, desired, observed, launch_json, created_at, created_by, ended_at, updated_at)
		VALUES (?, 'spc_a', 'nod_1', 1, 'worker', 'implementor', 'claude-code', 'ins_x', 'running', ?, ?, ?, 't', ?, ?)`, id, observed, string(launch), created, end, created)
}

func (f *fx) task(id, status, run string) {
	f.t.Helper()
	f.exec(`INSERT INTO tasks (id, space_id, title, rank, status, assignee_run, created_at, created_by, updated_at) VALUES (?, 'spc_a', ?, 1, ?, ?, 0, 't', 0)`, id, "Title "+id, status, run)
}

func (f *fx) report() {
	f.t.Helper()
	at := f.clk.Now().Add(-time.Minute)
	f.rp.OnStatus(f.ctx, "nod_1", proto.RepoStatusReport{CommandID: "cmd_x", SpaceID: "spc_a", CheckedAt: at, Repositories: []proto.RepoState{
		{ID: f.repo.ID, Path: "api", Present: true, LastFetchAt: &at, Check: &proto.RepoCheck{Path: "api", Branch: "main", Head: "aaaa",
			Branches: []proto.BranchCheck{{Name: "main", Commit: "aaaa"}, {Name: "spaceman/tsk_1", Commit: "bbbb"}, {Name: "spaceman/tsk_2", Commit: "cccc", Unpushed: 2}},
			Worktrees: []proto.WorktreeCheck{
				{Path: "worktrees/spaceman-tsk_1", Branch: "spaceman/tsk_1", Head: "bbbb", Status: proto.WorktreeStatusCached, StatusAt: &at},
				{Path: "worktrees/spaceman-tsk_2", Branch: "spaceman/tsk_2", Head: "cccc", Dirty: 1, Untracked: 2},
				{Path: "worktrees/hand", Branch: "hand", Head: "dddd"},
				{Path: "elsewhere/other", Branch: "o", Head: "eeee"},
			}}},
	}})
}

// commands returns the queued space.worktree.remove commands, oldest first, as id and payload.
func (f *fx) commands() (ids []string, payloads []proto.WorktreeRemovePayload) {
	f.t.Helper()
	rows, err := f.s.ReadDB().QueryContext(f.ctx, `SELECT id, payload_json FROM node_commands WHERE kind = ? ORDER BY created_at, id`, proto.CmdWorktreeRemove)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, p string
		var pl proto.WorktreeRemovePayload
		if err := rows.Scan(&id, &p); err != nil || json.Unmarshal([]byte(p), &pl) != nil {
			f.t.Fatalf("payload %q %v", p, err)
		}
		ids, payloads = append(ids, id), append(payloads, pl)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return ids, payloads
}

func (f *fx) answer(cmd, name, result, code, reason string) {
	f.svc.OnRemoved(f.ctx, "nod_1", proto.WorktreeRemoved{CommandID: cmd, SpaceID: "spc_a", Repo: "api", Name: name, Result: result, Code: code, Reason: reason,
		Path: "worktrees/" + name, Branch: "spaceman/tsk_1", Head: "bbbb", CheckedAt: f.clk.Now()})
}

func (f *fx) call(kind, tool string, args any, out any) error {
	f.t.Helper()
	b, _ := json.Marshal(args)
	raw, err := f.reg.Dispatch(f.ctx, &fleet.RunPrincipal{RunID: "run_c", SpaceID: "spc_a", Kind: kind, Role: "control", InstanceID: "ins_c"}, tool, b)
	if err == nil && out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			f.t.Fatal(err)
		}
	}
	return err
}

func code(err error) apperr.Code { return apperr.CodeOf(err) }

// ADR-0107 decision 3: the listing joins the node's report with the owning run and task, shows pushed as of the last
// fetch, and never invents a pull request state; only folders directly inside worktrees/ are listed.
func TestWorktreesList(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	l, err := f.svc.List(f.ctx, "spc_a", "")
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Entry{}
	for _, e := range l.Worktrees {
		by[e.Name] = e
	}
	if len(l.Worktrees) != 3 || l.ReportedAt == nil {
		t.Fatalf("listing %+v", l)
	}
	done := by["spaceman-tsk_1"]
	if !done.Owned || done.Run == nil || done.Run.ID != "run_done" || done.Run.Live || done.Task == nil || done.Task.ID != "tsk_1" || done.Task.Status != "done" ||
		done.Pushed == nil || !*done.Pushed || done.PushedAsOf == nil || done.LastChange == nil || !done.Prunable || done.CountsFrom != "cached" ||
		done.PullRequest == nil || done.PullRequest.State != "unknown" || done.Repo != "api" {
		t.Fatalf("finished worktree %+v", done)
	}
	live := by["spaceman-tsk_2"]
	if live.Run == nil || !live.Run.Live || live.Prunable || live.Pushed == nil || *live.Pushed || live.Dirty != 1 || live.Untracked != 2 || live.Task == nil || live.Task.Status != "running" {
		t.Fatalf("live worktree %+v", live)
	}
	hand := by["hand"]
	if hand.Owned || hand.Run != nil || hand.Task != nil || hand.Prunable || hand.LastChange != nil {
		t.Fatalf("unowned worktree %+v", hand)
	}
	if one, err := f.svc.List(f.ctx, "spc_a", "api"); err != nil || len(one.Worktrees) != 3 {
		t.Fatalf("one repository: %v %+v", err, one)
	}
	if _, err := f.svc.List(f.ctx, "spc_a", "nope"); code(err) != apperr.CodeNotFound {
		t.Fatalf("unknown repository: %v", err)
	}
	var viaTool Listing
	if err := f.call(agenttools.KindControl, "spaceman_worktrees_list", map[string]any{}, &viaTool); err != nil || len(viaTool.Worktrees) != 3 {
		t.Fatalf("tool: %v %+v", err, viaTool)
	}
}

// ADR-0107 decision 4: a removal is queued for the owning node with the configured repository (no key in the stored
// payload), the node's answer is recorded once and surfaced, and a removed worktree asks for a fresh report.
func TestRemoveHappyPath(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	done := make(chan *Outcome, 1)
	go func() {
		o, err := f.svc.Remove(f.ctx, admin, "spc_a", "api", "spaceman-tsk_1", 0)
		if err != nil {
			t.Error(err)
		}
		done <- o
	}()
	var cmd string
	for i := 0; i < 200 && cmd == ""; i++ {
		if ids, _ := f.commands(); len(ids) > 0 {
			cmd = ids[0]
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if cmd == "" {
		t.Fatal("no command queued")
	}
	_, payloads := f.commands()
	if payloads[0].Name != "spaceman-tsk_1" || payloads[0].Repository.ID != f.repo.ID || payloads[0].Repository.Path != "api" || payloads[0].Repository.SSHKey != "" {
		t.Fatalf("payload %+v", payloads[0])
	}
	// Not heard: another node, and a command of another kind.
	f.svc.OnRemoved(f.ctx, "nod_other", proto.WorktreeRemoved{CommandID: cmd, SpaceID: "spc_a", Name: "spaceman-tsk_1", Result: proto.WorktreeRemovedOK})
	if o, _ := f.svc.Result(f.ctx, "spc_a", cmd); o.Status != StatusPending {
		t.Fatalf("an answer from another node was kept: %+v", o)
	}
	f.answer(cmd, "spaceman-tsk_1", proto.WorktreeRemovedOK, "", "")
	o := <-done
	if o.Status != StatusRemoved || o.CommandID != cmd || o.By != "node" || o.Branch != "spaceman/tsk_1" || o.Head != "bbbb" {
		t.Fatalf("outcome %+v", o)
	}
	// The first answer is kept.
	f.answer(cmd, "spaceman-tsk_1", proto.WorktreeRefused, "dirty", "later")
	if o, _ := f.svc.Result(f.ctx, "spc_a", cmd); o.Status != StatusRemoved || o.Code != "" {
		t.Fatalf("second answer replaced the first: %+v", o)
	}
	// Both events are in the audit log, and the removal asked for a report without fetching.
	var n int
	_ = f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE target_type = ? AND target_id = ?`, TargetType, cmd).Scan(&n)
	if n != 2 {
		t.Fatalf("audit events %d", n)
	}
	var syncs int
	_ = f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM node_commands WHERE kind = ?`, proto.CmdReposSync).Scan(&syncs)
	if syncs < 2 {
		t.Fatalf("no report refresh queued after the removal (%d sync commands)", syncs)
	}
	if _, err := f.svc.Result(f.ctx, "spc_b", cmd); code(err) != apperr.CodeNotFound {
		t.Fatalf("another space's command: %v", err)
	}
}

// ADR-0107 decision 1: every refusal code the node answers with, and an error, reach the caller verbatim.
func TestRemoveRefusalCodesPassThrough(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	for _, c := range proto.WorktreeRefusalCodes {
		o, err := f.svc.Request(f.ctx, admin, "spc_a", "api", "spaceman-tsk_1")
		if err != nil || o.Status != StatusPending {
			t.Fatalf("%s: %v %+v", c, err, o)
		}
		f.answer(o.CommandID, "spaceman-tsk_1", proto.WorktreeRefused, c, "reason for "+c)
		got, err := f.svc.Await(f.ctx, "spc_a", o, time.Second)
		if err != nil || got.Status != StatusRefused || got.Code != c || got.Reason != "reason for "+c || got.By != "node" {
			t.Fatalf("%s: %v %+v", c, err, got)
		}
	}
	o, _ := f.svc.Request(f.ctx, admin, "spc_a", "api", "spaceman-tsk_1")
	f.answer(o.CommandID, "spaceman-tsk_1", proto.WorktreeFailed, "", "git is broken")
	if got, _ := f.svc.Result(f.ctx, "spc_a", o.CommandID); got.Status != StatusError || got.Reason != "git is broken" {
		t.Fatalf("error outcome %+v", got)
	}
	// An unanswered request comes back pending with its command ID.
	f.svc.Wait = 30 * time.Millisecond
	got, err := f.svc.Remove(f.ctx, admin, "spc_a", "api", "spaceman-tsk_1", 0)
	if err != nil || got.Status != StatusPending || got.CommandID == "" {
		t.Fatalf("unanswered: %v %+v", err, got)
	}
}

// ADR-0107 decision 5: central refuses a worktree a live run holds before asking the node, with the node's own code.
func TestRemoveRefusedForLiveRun(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	o, err := f.svc.Remove(f.ctx, admin, "spc_a", "api", "worktrees/spaceman-tsk_2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if o.Status != StatusRefused || o.Code != CodeLiveRun || o.By != "central" || o.CommandID != "" || !strings.Contains(o.Reason, "run_live") {
		t.Fatalf("outcome %+v", o)
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatalf("a command was queued for a worktree a live run holds: %v", ids)
	}
	var n int
	_ = f.s.ReadDB().QueryRowContext(f.ctx, `SELECT count(*) FROM audit_events WHERE action = ? AND outcome = 'denied'`, ActionRefused).Scan(&n)
	if n != 1 {
		t.Fatalf("audit refusals %d", n)
	}
	// Once the run has ended the worktree is asked for.
	f.exec(`UPDATE runs SET observed = 'exited' WHERE id = 'run_live'`)
	if o, err := f.svc.Request(f.ctx, admin, "spc_a", "api", "spaceman-tsk_2"); err != nil || o.Status != StatusPending {
		t.Fatalf("after the run ended: %v %+v", err, o)
	}
}

// ADR-0107 decision 4: a node below protocol minor 16 is refused with a clear error and nothing is queued; a missing
// lease, an unknown repository, an unlisted or malformed worktree name are errors too.
func TestRemoveRefusals(t *testing.T) {
	old := newFx(t, proto.MinorWorktreeRemove-1)
	if _, err := old.svc.Request(old.ctx, admin, "spc_a", "api", "spaceman-tsk_1"); code(err) != apperr.CodePrecondition || !strings.Contains(err.Error(), "upgrade") || !strings.Contains(err.Error(), "1.16") {
		t.Fatalf("old node: %v", err)
	}
	if ids, _ := old.commands(); len(ids) != 0 {
		t.Fatal("command queued for an old node")
	}
	f := newFx(t, proto.MinorWorktreeRemove)
	for name, tc := range map[string]struct {
		repo, wt string
		want     apperr.Code
	}{
		"unknown repository": {"nope", "spaceman-tsk_1", apperr.CodeNotFound},
		"not in the report":  {"api", "ghost", apperr.CodeNotFound},
		"outside worktrees":  {"api", "other", apperr.CodeNotFound},
		"nested name":        {"api", "a/b", apperr.CodeInvalid},
		"dotdot":             {"api", "..", apperr.CodeInvalid},
		"empty":              {"api", "", apperr.CodeInvalid},
	} {
		if _, err := f.svc.Request(f.ctx, admin, "spc_a", tc.repo, tc.wt); code(err) != tc.want {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.svc.Request(f.ctx, admin, "spc_none", "api", "x"); code(err) != apperr.CodeNotFound {
		t.Errorf("unknown space: %v", err)
	}
	f.exec(`UPDATE execution_leases SET state = 'revoked' WHERE space_id = 'spc_a'`)
	if _, err := f.svc.Request(f.ctx, admin, "spc_a", "api", "spaceman-tsk_1"); code(err) != apperr.CodeUnavailable {
		t.Errorf("no lease: %v", err)
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatalf("commands queued by refused requests: %v", ids)
	}
}

// ADR-0107 decision 5: the three tools are for control runs only.
func TestWorktreeToolsControlOnly(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	for _, kind := range []string{agenttools.KindWorker, agenttools.KindKB, agenttools.KindDreamer, agenttools.KindDiagnostic} {
		for tool, args := range map[string]any{
			"spaceman_worktrees_list":  map[string]any{},
			"spaceman_worktree_remove": map[string]any{"repo": "api", "name": "spaceman-tsk_1"},
			"spaceman_worktrees_prune": map[string]any{"apply": true},
		} {
			if err := f.call(kind, tool, args, nil); code(err) != apperr.CodeForbidden {
				t.Errorf("%s may call %s: %v", kind, tool, err)
			}
		}
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatalf("a non-control caller queued commands: %v", ids)
	}
	for _, tl := range f.reg.Catalog(agenttools.KindWorker) {
		if strings.HasPrefix(tl.Name, "spaceman_worktree") {
			t.Errorf("worker catalog lists %s", tl.Name)
		}
	}
	names := map[string]bool{}
	for _, tl := range f.reg.Catalog(agenttools.KindControl) {
		names[tl.Name] = true
	}
	for _, n := range []string{"spaceman_worktrees_list", "spaceman_worktree_remove", "spaceman_worktrees_prune"} {
		if !names[n] {
			t.Errorf("control catalog lacks %s", n)
		}
	}
}

// The control tools: remove answers with the node's code, a live run is refused, bad input is invalid, and a pending
// removal is asked about with its command_id.
func TestWorktreeRemoveTool(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.Wait = 30 * time.Millisecond
	var o Outcome
	if err := f.call(agenttools.KindControl, "spaceman_worktree_remove", map[string]any{"repo": "api", "name": "spaceman-tsk_1"}, &o); err != nil || o.Status != StatusPending || o.CommandID == "" {
		t.Fatalf("pending: %v %+v", err, o)
	}
	f.answer(o.CommandID, "spaceman-tsk_1", proto.WorktreeRefused, "unpushed", "HEAD is not on the remote")
	var ans Outcome
	if err := f.call(agenttools.KindControl, "spaceman_worktree_remove", map[string]any{"command_id": o.CommandID}, &ans); err != nil || ans.Status != StatusRefused || ans.Code != "unpushed" {
		t.Fatalf("command_id: %v %+v", err, ans)
	}
	var live Outcome
	if err := f.call(agenttools.KindControl, "spaceman_worktree_remove", map[string]any{"repo": "api", "name": "spaceman-tsk_2"}, &live); err != nil || live.Code != CodeLiveRun {
		t.Fatalf("live: %v %+v", err, live)
	}
	for _, in := range []map[string]any{{}, {"repo": "api"}, {"name": "x"}, {"command_id": o.CommandID, "repo": "api"}} {
		if err := f.call(agenttools.KindControl, "spaceman_worktree_remove", in, nil); code(err) != apperr.CodeInvalid {
			t.Errorf("%v: %v", in, err)
		}
	}
	if err := f.call(agenttools.KindControl, "spaceman_worktree_remove", map[string]any{"command_id": "cmd_nope"}, nil); code(err) != apperr.CodeNotFound {
		t.Errorf("unknown command: %v", err)
	}
	if err := f.call(agenttools.KindControl, "spaceman_worktree_remove", map[string]any{"repo": "api", "name": "x", "force": true}, nil); code(err) != apperr.CodeInvalid {
		t.Errorf("a force argument is accepted: %v", err)
	}
}

// ADR-0107 decisions 4 and 5: prune picks only finished, owned worktrees (done or cancelled task, ended run, old
// enough), lists them without apply, removes through the same request with apply, and respects the limit.
func TestWorktreesPrune(t *testing.T) {
	f := newFx(t, proto.MinorWorktreeRemove)
	f.svc.Wait = 30 * time.Millisecond
	var dry PruneResult
	if err := f.call(agenttools.KindControl, "spaceman_worktrees_prune", map[string]any{}, &dry); err != nil || dry.Applied || dry.Candidates != 1 || len(dry.Would) != 1 || dry.Would[0].Name != "spaceman-tsk_1" {
		t.Fatalf("dry run: %v %+v", err, dry)
	}
	if ids, _ := f.commands(); len(ids) != 0 {
		t.Fatal("a dry run queued commands")
	}
	var none PruneResult
	if err := f.call(agenttools.KindControl, "spaceman_worktrees_prune", map[string]any{"older_than_days": 30}, &none); err != nil || none.Candidates != 0 {
		t.Fatalf("too young: %v %+v", err, none)
	}
	// A failed task, an open task and an unowned worktree are never picked.
	f.exec(`UPDATE tasks SET status = 'failed' WHERE id = 'tsk_1'`)
	if c, _ := f.svc.Candidates(f.ctx, "spc_a", "", 0, 0); len(c) != 0 {
		t.Fatalf("failed task picked: %+v", c)
	}
	f.exec(`UPDATE tasks SET status = 'cancelled' WHERE id = 'tsk_1'`)
	var res PruneResult
	if err := f.call(agenttools.KindControl, "spaceman_worktrees_prune", map[string]any{"apply": true, "older_than_days": 7}, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Applied || len(res.Results) != 1 || res.Results[0].Status != StatusPending || res.Results[0].Name != "spaceman-tsk_1" {
		t.Fatalf("apply: %+v", res)
	}
	if ids, _ := f.commands(); len(ids) != 1 {
		t.Fatalf("commands %v", ids)
	}
	f.answer(res.Results[0].CommandID, "spaceman-tsk_1", proto.WorktreeRefused, "dirty", "changed files")
	if o, _ := f.svc.Result(f.ctx, "spc_a", res.Results[0].CommandID); o.Code != "dirty" {
		t.Fatalf("refusal not recorded: %+v", o)
	}
	for _, in := range []map[string]any{{"limit": 26}, {"older_than_days": -1}, {"older_than_days": 40000}} {
		if err := f.call(agenttools.KindControl, "spaceman_worktrees_prune", in, nil); err == nil {
			t.Errorf("%v accepted", in)
		}
	}
}
