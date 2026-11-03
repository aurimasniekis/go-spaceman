package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aurimasniekis/go-spaceman/internal/audit"
	"github.com/aurimasniekis/go-spaceman/internal/fleet"
	hns "github.com/aurimasniekis/go-spaceman/internal/harness"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/queue"
	"github.com/aurimasniekis/go-spaceman/internal/repos"
)

// SM-REPO-001/002/003, SM-SPACE-001, ADR-0044 end to end with a local bare repository as the remote: a repository is
// configured over the API (If-Match, refusal of credentials in URLs), the node clones it while it holds the lease,
// control launches a worker in a worktree of it by name, an on_demand repository is cloned at its first launch, a sync
// fetches and reports the remote, heads, changes, untracked files, unpushed commits and the worktree's owner, agents
// read it through spaceman_repos_list, and removing a repository leaves its folder.
func TestSpaceRepositoriesEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	e := newFleetEnv(t)
	e.enroll()
	e.startAgent()
	ctx := context.Background()
	admin := audit.Actor{Type: audit.ActorUser, ID: "usr_admin"}
	nodes, _ := e.c.Fleet.ListNodes(ctx)
	eventually(t, "node connected", func() bool { n, _ := e.c.Fleet.GetNode(ctx, nodes[0].ID); return n.Connected })
	sp, err := e.c.Fleet.CreateSpace(ctx, admin, fleet.CreateSpaceInput{Slug: "repos", Name: "Repos", NodeID: nodes[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	bindStubAccounts(t, e.c, sp.ID)
	eventually(t, "lease granted", func() bool { s, _ := e.c.Fleet.GetSpace(ctx, sp.ID); return s.Lease != nil && s.Lease.Valid })

	// The remote: a bare repository with one commit on main.
	root := t.TempDir()
	bare, seed := filepath.Join(root, "api.git"), filepath.Join(root, "seed")
	gitIn(t, root, "init", "-q", "--bare", "-b", "main", bare)
	gitIn(t, root, "clone", "-q", bare, seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "checkout", "-q", "-b", "main")
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "init")
	gitIn(t, seed, "push", "-q", "-u", "origin", "main")
	head := gitIn(t, seed, "rev-parse", "HEAD")

	rootSecret, _, err := e.c.Auth.IssueRootToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path string, body any, hdr ...string) (int, http.Header, []byte) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, e.srv.URL+"/api/v1/spaces/"+sp.ID+path, rd)
		req.Header.Set("Authorization", "Bearer "+rootSecret)
		req.Header.Set("Content-Type", "application/json")
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, res.Header, b
	}
	list := func() repos.Overview {
		t.Helper()
		code, _, b := call("GET", "/repositories", nil)
		var ov repos.Overview
		if code != 200 || json.Unmarshal(b, &ov) != nil {
			t.Fatalf("list %d %s", code, b)
		}
		return ov
	}
	byName := func(ov repos.Overview, name string) *repos.Repository {
		for _, r := range ov.Items {
			if r.Name == name {
				return r
			}
		}
		return nil
	}

	if code, _, b := call("POST", "/repositories", map[string]any{"name": "api", "url": "https://user:secret@git.example.com/api.git"}); code != 422 ||
		bytes.Contains(b, []byte("secret")) {
		t.Fatalf("credentials in the URL: %d %s", code, b)
	}
	if code, _, b := call("POST", "/repositories", map[string]any{"name": "api", "url": "ext::sh%-c%id"}); code != 422 {
		t.Fatalf("remote helper: %d %s", code, b)
	}
	code, hdr, b := call("POST", "/repositories", map[string]any{"name": "api", "path": "repos/api", "url": bare, "description": "The API"})
	if code != 201 || hdr.Get("ETag") != `"1"` {
		t.Fatalf("create %d %s", code, b)
	}
	var api repos.Repository
	_ = json.Unmarshal(b, &api)

	// auto: the connected node clones it at once and reports it.
	clone := filepath.Join(e.agentDir, "spaces", sp.Workdir, "repos", "api")
	eventually(t, "cloned and reported", func() bool {
		r := byName(list(), "api")
		return r != nil && r.Status != nil && r.Status.State == "present" && r.Status.DefaultHead == head
	})
	if gitIn(t, clone, "rev-parse", "HEAD") != head {
		t.Fatal("clone head")
	}
	st := byName(list(), "api").Status
	if st.Remote == nil || !st.Remote.Reachable || st.Remote.Head != head || st.LastFetchAt == nil || st.Dirty != 0 {
		t.Fatalf("status %+v", st)
	}

	// Control launches a worker in a worktree of the repository, named by its name.
	ctrl, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "control", Role: "control", Harness: hns.KindClaude})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "control running", func() bool { return e.run1(ctrl.ID).Observed == "running" })
	ct := e.runToken(ctrl.ID)
	task, _, err := e.c.Queue.Create(ctx, admin, sp.ID, queue.CreateInput{Title: "Add a health check", Status: queue.Ready})
	if err != nil {
		t.Fatal(err)
	}
	res := e.bridgeCall(ctrl.ID, ct, "spaceman_worker_launch", map[string]any{"task_id": task.ID, "role": "implementor", "repo": "api"})
	if !res.OK {
		t.Fatalf("launch: %+v", res)
	}
	var w struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(res.Result, &w)
	eventually(t, "worker running", func() bool { return e.run1(w.RunID).Observed == "running" })
	worker := e.run1(w.RunID)
	if worker.Launch.Worktree == nil || worker.Launch.Worktree.Repo != "repos/api" || !strings.Contains(string(worker.Manifest), api.ID) {
		t.Fatalf("worker launch %+v manifest %s", worker.Launch.Worktree, worker.Manifest)
	}
	wt := filepath.Join(e.agentDir, "spaces", sp.Workdir, "worktrees", "spaceman-"+task.ID)
	if gitIn(t, wt, "branch", "--show-current") != "spaceman/"+task.ID {
		t.Fatal("worktree branch")
	}

	// The worker's work: an unpushed commit and an untracked file. Upstream moves on.
	if err := os.WriteFile(filepath.Join(wt, "health.go"), []byte("package api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", ".")
	gitIn(t, wt, "commit", "-q", "-m", "health check")
	if err := os.WriteFile(filepath.Join(wt, "notes.txt"), []byte("todo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "CHANGES.md"), []byte("upstream\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitIn(t, seed, "add", ".")
	gitIn(t, seed, "commit", "-q", "-m", "upstream")
	gitIn(t, seed, "push", "-q")
	upstream := gitIn(t, seed, "rev-parse", "HEAD")

	// An explicit sync fetches and reports everything.
	code, _, b = call("POST", "/repositories/sync", map[string]any{})
	var sync repos.SyncResult
	if code != 202 || json.Unmarshal(b, &sync) != nil || sync.CommandID == "" || len(sync.Fetch) != 1 {
		t.Fatalf("sync %d %s", code, b)
	}
	eventually(t, "fetched and reported", func() bool {
		ov := list()
		r := byName(ov, "api")
		return !ov.SyncPending && r.Status != nil && r.Status.TrackingHead == upstream && r.Status.Fetched
	})
	st = byName(list(), "api").Status
	if st.DefaultHead != head || st.Unpushed < 1 || len(st.Worktrees) != 1 {
		t.Fatalf("after sync %+v", st)
	}
	if wtst := st.Worktrees[0]; wtst.RunID != w.RunID || wtst.Untracked != 1 || wtst.Unpushed != 1 || wtst.Branch != "spaceman/"+task.ID {
		t.Fatalf("worktree %+v", wtst)
	}
	if gitIn(t, clone, "rev-parse", "main") != head {
		t.Fatal("the sync moved a local branch")
	}

	// Agents read the same view, read only.
	res = e.bridgeCall(ctrl.ID, ct, "spaceman_repos_list", nil)
	if !res.OK || !strings.Contains(string(res.Result), `"repos/api"`) || !strings.Contains(string(res.Result), upstream) {
		t.Fatalf("repos tool: %+v", res)
	}

	// on_demand: not cloned until a run needs it.
	code, _, b = call("POST", "/repositories", map[string]any{"name": "lazy", "url": bare, "clone_policy": "on_demand"})
	if code != 201 {
		t.Fatalf("create lazy %d %s", code, b)
	}
	lazyDir := filepath.Join(e.agentDir, "spaces", sp.Workdir, "lazy")
	eventually(t, "lazy reported missing", func() bool {
		r := byName(list(), "lazy")
		return r != nil && r.Status != nil && r.Status.State == "missing"
	})
	run, err := e.c.Fleet.LaunchRun(ctx, admin, sp.ID, fleet.LaunchInput{Kind: "worker", Role: "implementor", Harness: hns.KindClaude,
		Worktree: &proto.Worktree{Repo: "lazy", Branch: "spaceman/lazy-1"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "lazy worker running", func() bool { return e.run1(run.ID).Observed == "running" })
	if gitIn(t, lazyDir, "rev-parse", "HEAD") != upstream {
		t.Fatal("cloned at first launch")
	}

	// Optimistic concurrency on edits.
	upd := map[string]any{"url": bare, "default_branch": "main", "description": "The API service"}
	if code, _, _ := call("PUT", "/repositories/api", upd); code != 428 {
		t.Fatalf("update without If-Match: %d", code)
	}
	if code, _, _ := call("PUT", "/repositories/api", upd, "If-Match", `"7"`); code != 409 {
		t.Fatalf("update with a stale version: %d", code)
	}
	if code, _, _ := call("PUT", "/repositories/api", map[string]any{"path": "elsewhere", "url": bare}, "If-Match", `"1"`); code != 422 {
		t.Fatalf("path change: %d", code)
	}
	code, hdr, b = call("PUT", "/repositories/api", upd, "If-Match", `"1"`)
	if code != 200 || hdr.Get("ETag") != `"2"` {
		t.Fatalf("update %d %s", code, b)
	}

	// Removing a repository removes the configuration only.
	if code, _, _ := call("DELETE", "/repositories/lazy", nil); code != 204 {
		t.Fatalf("delete %d", code)
	}
	if byName(list(), "lazy") != nil {
		t.Fatal("still listed")
	}
	if _, err := os.Stat(filepath.Join(lazyDir, ".git")); err != nil {
		t.Fatalf("the folder was removed: %v", err)
	}
}
