package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/ids"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// wtCmd builds a space.worktree.remove command of the env's space and repository.
func (e *rmEnv) wtCmd(t *testing.T, name string) proto.Command {
	t.Helper()
	b, err := json.Marshal(proto.WorktreeRemovePayload{Repository: e.src, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	id := ids.New(ids.Command)
	return proto.Command{CommandID: id, Kind: proto.CmdWorktreeRemove, SpaceID: e.spaceID, Generation: 1, IdempotencyKey: "wt:" + id, Payload: b}
}

// grantLease gives the agent a lease of the env's space whose folder is the env's space folder.
func (e *rmEnv) grantLease(t *testing.T) {
	t.Helper()
	rel, err := filepath.Rel(filepath.Join(e.a.o.DataDir, "spaces"), e.space)
	if err != nil {
		t.Fatal(err)
	}
	e.a.mu.Lock()
	e.a.leases = map[string]leaseRow{e.spaceID: {SpaceID: e.spaceID, Generation: 1, ExpiresAt: time.Now().Add(time.Hour), Workdir: rel}}
	e.a.mu.Unlock()
}

// report runs the command through the agent's command path and returns the journaled report once it is done.
func (e *rmEnv) report(t *testing.T, c proto.Command) proto.WorktreeRemoved {
	t.Helper()
	e.a.command(context.Background(), c)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var state, reason string
		err := e.a.j.s.ReadDB().QueryRowContext(context.Background(), `SELECT state, reason FROM commands WHERE id = ?`, c.CommandID).Scan(&state, &reason)
		if err == nil && state == "done" {
			var rep proto.WorktreeRemoved
			if err := json.Unmarshal([]byte(reason), &rep); err != nil {
				t.Fatalf("journaled report %q: %v", reason, err)
			}
			if err := rep.Validate(); err != nil {
				t.Fatalf("report %+v: %v", rep, err)
			}
			return rep
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the command did not finish")
	return proto.WorktreeRemoved{}
}

// ADR-0107 decision 4: space.worktree.remove removes a clean, pushed worktree through the command path and the journaled
// report says so; the branch stays. A repeated delivery removes nothing again.
func TestWorktreeRemoveCommandRemoves(t *testing.T) {
	e := newRmEnv(t)
	e.grantLease(t)
	dir := e.worktree(t, "spaceman/pushed", true)
	c := e.wtCmd(t, "spaceman-pushed")
	rep := e.report(t, c)
	if rep.Result != proto.WorktreeRemovedOK || rep.Code != "" || rep.CommandID != c.CommandID || rep.SpaceID != e.spaceID ||
		rep.Repo != "repos/api" || rep.Name != "spaceman-pushed" || rep.Path != "worktrees/spaceman-pushed" || rep.Branch != "spaceman/pushed" ||
		len(rep.Head) < 40 || rep.CheckedAt.IsZero() {
		t.Fatalf("report %+v", rep)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("worktree still there: %v", err)
	}
	gitRun(t, e.repo, "rev-parse", "--verify", "refs/heads/spaceman/pushed")

	// The same command delivered again is a duplicate: it never runs the removal, even with a removable worktree of
	// that name there again.
	again, err := prepareWorktree(context.Background(), e.space, proto.Worktree{Repo: "repos/api", Branch: "spaceman/pushed"})
	if err != nil {
		t.Fatal(err)
	}
	e.a.command(context.Background(), c)
	time.Sleep(300 * time.Millisecond)
	mustExist(t, again)
}

// ADR-0107 decision 4: every refusal of the safe-removal path comes back as a refused report with its code, and nothing
// is removed.
func TestWorktreeRemoveCommandReportsEachRefusal(t *testing.T) {
	e := newRmEnv(t)
	e.grantLease(t)
	pushed := e.worktree(t, "spaceman/pushed", true)

	cases := []struct {
		name string
		code string
		set  func(t *testing.T) (name string, undo func())
	}{
		{"invalid", RefuseInvalid, func(*testing.T) (string, func()) { return "..", func() {} }},
		{"outside", RefuseOutside, func(*testing.T) (string, func()) { return "nope", func() {} }},
		{"not_worktree", RefuseNotWorktree, func(t *testing.T) (string, func()) {
			// A folder in worktrees/ that git does not list for this repository.
			d := filepath.Join(e.space, "worktrees", "stray")
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Fatal(err)
			}
			return "stray", func() { _ = os.Remove(d) }
		}},
		{"locked", RefuseLocked, func(t *testing.T) (string, func()) {
			gitRun(t, e.repo, "worktree", "lock", pushed)
			return "spaceman-pushed", func() { gitRun(t, e.repo, "worktree", "unlock", pushed) }
		}},
		{"no_remote", RefuseNoRemote, func(*testing.T) (string, func()) {
			url := e.src.URL
			e.src.URL = ""
			return "spaceman-pushed", func() { e.src.URL = url }
		}},
		{"fetch_failed", RefuseFetch, func(*testing.T) (string, func()) {
			url := e.src.URL
			e.src.URL = filepath.Join(t.TempDir(), "missing.git")
			return "spaceman-pushed", func() { e.src.URL = url }
		}},
		{"live_run", RefuseLiveRun, func(t *testing.T) (string, func()) {
			id := e.liveRun(t, e.spaceID, pushed)
			return "spaceman-pushed", func() { e.endRun(t, id) }
		}},
		{"dirty", RefuseDirty, func(t *testing.T) (string, func()) {
			writeFile(t, filepath.Join(pushed, "work.txt"), "changed\n")
			return "spaceman-pushed", func() { gitRun(t, pushed, "checkout", "--", "work.txt") }
		}},
		{"untracked", RefuseUntracked, func(t *testing.T) (string, func()) {
			f := filepath.Join(pushed, "new.txt")
			writeFile(t, f, "new\n")
			return "spaceman-pushed", func() { _ = os.Remove(f) }
		}},
		{"unpushed", RefuseUnpushed, func(t *testing.T) (string, func()) {
			writeFile(t, filepath.Join(pushed, "more.txt"), "more\n")
			gitRun(t, pushed, "add", "more.txt")
			gitRun(t, pushed, "commit", "-q", "-m", "more")
			return "spaceman-pushed", func() { gitRun(t, pushed, "reset", "-q", "--hard", "HEAD~1") }
		}},
		{"git_error", RefuseGit, func(t *testing.T) (string, func()) {
			// The worktree's link to its repository is broken, so git cannot read its state.
			link := filepath.Join(pushed, ".git")
			b, err := os.ReadFile(link)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(link, []byte("gitdir: "+filepath.Join(t.TempDir(), "gone")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return "spaceman-pushed", func() { _ = os.WriteFile(link, b, 0o600) }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, undo := tc.set(t)
			defer undo()
			rep := e.report(t, e.wtCmd(t, name))
			if rep.Result != proto.WorktreeRefused || rep.Code != tc.code || rep.Reason == "" {
				t.Fatalf("report %+v, want refusal %q", rep, tc.code)
			}
			if tc.code != RefuseInvalid && tc.code != RefuseOutside && tc.code != RefuseNotWorktree {
				mustExist(t, pushed)
			}
		})
	}
	// Nothing above removed it, and with every cause undone the same command now removes it.
	mustExist(t, pushed)
	if rep := e.report(t, e.wtCmd(t, "spaceman-pushed")); rep.Result != proto.WorktreeRemovedOK {
		t.Fatalf("after undoing every cause: %+v", rep)
	}
}

// A malformed payload is rejected before anything runs, and a command without a valid lease is rejected.
func TestWorktreeRemoveCommandRejectsBadInput(t *testing.T) {
	e := newRmEnv(t)
	e.grantLease(t)
	dir := e.worktree(t, "spaceman/pushed", true)
	for name, mut := range map[string]func(*proto.WorktreeRemovePayload){
		"no id":     func(p *proto.WorktreeRemovePayload) { p.Repository.ID = "" },
		"no path":   func(p *proto.WorktreeRemovePayload) { p.Repository.Path = "" },
		"no name":   func(p *proto.WorktreeRemovePayload) { p.Name = "" },
		"slash":     func(p *proto.WorktreeRemovePayload) { p.Name = "a/b" },
		"backslash": func(p *proto.WorktreeRemovePayload) { p.Name = "a\\b" },
	} {
		p := proto.WorktreeRemovePayload{Repository: e.src, Name: "spaceman-pushed"}
		mut(&p)
		b, _ := json.Marshal(p)
		c := e.wtCmd(t, "x")
		c.Payload = b
		e.a.command(context.Background(), c)
		var state string
		if err := e.a.j.s.ReadDB().QueryRowContext(context.Background(), `SELECT state FROM commands WHERE id = ?`, c.CommandID).Scan(&state); err != nil || state != "rejected" {
			t.Errorf("%s: state %q %v", name, state, err)
		}
	}
	e.a.mu.Lock()
	e.a.leases = nil
	e.a.mu.Unlock()
	c := e.wtCmd(t, "spaceman-pushed")
	e.a.command(context.Background(), c)
	var state string
	if err := e.a.j.s.ReadDB().QueryRowContext(context.Background(), `SELECT state FROM commands WHERE id = ?`, c.CommandID).Scan(&state); err != nil || state != "rejected" {
		t.Errorf("no lease: state %q %v", state, err)
	}
	mustExist(t, dir)
}
