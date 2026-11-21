package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// execLog puts a git wrapper first on PATH that records every exec's arguments, and returns the recorded lines.
func execLog(t *testing.T) func() []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + log + "'\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // test wrapper
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		b, _ := os.ReadFile(log) //nolint:gosec // test file
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

func statusExecs(lines []string) (status int) {
	for _, l := range lines {
		if l == "" {
			continue
		}
		if strings.Contains(l, " status ") {
			status++
		}
	}
	return status
}

// age sets a worktree's visible activity (its folder, entries, index and HEAD) to when.
func age(t *testing.T, wt string, when time.Time) {
	t.Helper()
	gd := strings.TrimPrefix(strings.TrimSpace(readText(t, filepath.Join(wt, ".git"))), "gitdir: ")
	paths := []string{wt, filepath.Join(gd, "index"), filepath.Join(gd, "HEAD")}
	ents, err := os.ReadDir(wt)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != ".git" {
			paths = append(paths, filepath.Join(wt, e.Name()))
		}
	}
	for _, p := range paths {
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ADR-0106 decision 8 (WL-6): a repository report's git status execs are bounded. Worktrees idle for over a week reuse
// cached counts, at most repoStatusMax status calls run per report, cached and unchecked worktrees say so, and a worktree
// whose status is unknown is never reported clean. Temporary repositories only.
func TestRepoReportWorktreeStatusBounded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	space, bare, _ := reposFixture(t)
	a := testAgent()
	ctx := context.Background()
	oldMax := repoStatusMax
	repoStatusMax = 5
	t.Cleanup(func() { repoStatusMax = oldMax })
	api := proto.RepoSource{ID: "rep_api", Name: "api", Path: "repos/api", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneAuto}
	sync := func(clone bool) proto.RepoCheck {
		p := proto.ReposSyncPayload{Repositories: []proto.RepoSource{api}}
		if clone {
			p.Clone = []string{"rep_api"}
		}
		rep := a.syncRepos(ctx, "spc_1", space, p)
		if len(rep.Repositories) != 1 || rep.Repositories[0].Check == nil {
			t.Fatalf("report %+v", rep)
		}
		return *rep.Repositories[0].Check
	}
	sync(true)
	repo := filepath.Join(space, "repos", "api")
	const n = 12
	var wts []string
	old := time.Now().Add(-10 * 24 * time.Hour)
	for i := range n {
		wt := filepath.Join(space, "worktrees", fmt.Sprintf("w%02d", i))
		gitRun(t, repo, "worktree", "add", "-q", "-b", fmt.Sprintf("b%02d", i), wt)
		wts = append(wts, wt)
	}
	for _, wt := range wts {
		age(t, wt, old)
	}
	logs := execLog(t)
	byStatus := func(c proto.RepoCheck) map[string]int {
		m := map[string]int{}
		for _, w := range c.Worktrees {
			m[w.Status]++
			if w.Status == proto.WorktreeStatusUnchecked && (w.Error == "" || w.Dirty != 0 || w.Untracked != 0) {
				t.Fatalf("an unchecked worktree must carry an error and no counts, never read as clean: %+v", w)
			}
			if w.Status == proto.WorktreeStatusCached && (w.StatusAt == nil || w.Error != "") {
				t.Fatalf("cached without its time: %+v", w)
			}
		}
		return m
	}
	prev := 0
	report := func() (proto.RepoCheck, int) {
		c := sync(false)
		st := statusExecs(logs()[prev:])
		prev = len(logs())
		return c, st
	}

	// Report 1: 12 idle worktrees, nothing cached: the cap (5) bounds the worktree statuses, the rest is unknown.
	c, st := report()
	if st > repoStatusMax+1 { // plus the repository's own status
		t.Fatalf("report 1 ran %d status execs, limit %d+1", st, repoStatusMax)
	}
	if m := byStatus(c); m[proto.WorktreeStatusFresh] != 5 || m[proto.WorktreeStatusUnchecked] != 7 {
		t.Fatalf("report 1: %v", m)
	}
	// Reports 2 and 3 fill the cache, each within the cap.
	for range 2 {
		if _, st = report(); st > repoStatusMax+1 {
			t.Fatalf("status execs %d over the limit", st)
		}
	}
	// Report 4: everything is cached, so only the repository's own status runs.
	c, st = report()
	if m := byStatus(c); m[proto.WorktreeStatusCached] != n {
		t.Fatalf("report 4: %v", m)
	}
	if st != 1 {
		t.Fatalf("a fully cached report ran %d status execs, want 1 (the repository's own)", st)
	}

	// A worktree that changes is checked again, and what it shows is real.
	if err := os.WriteFile(filepath.Join(wts[3], "new.txt"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, st = report()
	if st != 2 {
		t.Fatalf("one changed worktree: %d status execs, want 2", st)
	}
	for _, w := range c.Worktrees {
		if strings.HasSuffix(w.Path, "w03") && (w.Status != proto.WorktreeStatusFresh || w.Untracked != 1) {
			t.Fatalf("changed worktree: %+v", w)
		}
	}
}

// An old cached entry is refreshed with the allowance a report has left, and relocation's inspection stays exact: it
// checks every worktree however many there are.
func TestRepoReportWorktreeStatusRefreshAndExact(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	space, bare, _ := reposFixture(t)
	a := testAgent()
	ctx := context.Background()
	oldMax := repoStatusMax
	repoStatusMax = 3
	t.Cleanup(func() { repoStatusMax = oldMax })
	api := proto.RepoSource{ID: "rep_api", Name: "api", Path: "repos/api", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneAuto}
	p := proto.ReposSyncPayload{Repositories: []proto.RepoSource{api}, Clone: []string{"rep_api"}}
	check := func() proto.RepoCheck {
		rep := a.syncRepos(ctx, "spc_1", space, p)
		p.Clone = nil
		return *rep.Repositories[0].Check
	}
	check()
	repo := filepath.Join(space, "repos", "api")
	old := time.Now().Add(-10 * 24 * time.Hour)
	const n = 6
	for i := range n {
		wt := filepath.Join(space, "worktrees", fmt.Sprintf("w%d", i))
		gitRun(t, repo, "worktree", "add", "-q", "-b", fmt.Sprintf("b%d", i), wt)
		age(t, wt, old)
	}
	check()
	check()      // 6 worktrees, 3 a report: all checked after two reports
	c := check() // and cached in the third
	logs := execLog(t)
	for _, w := range c.Worktrees {
		if w.Status != proto.WorktreeStatusCached {
			t.Fatalf("not cached: %+v", w)
		}
	}
	// Age the cached entries past the refresh interval: the next report refreshes up to its allowance (3), not all.
	a.wtStatus.mu.Lock()
	for k, v := range a.wtStatus.m {
		v.at = v.at.Add(-2 * repoStatusRefresh)
		a.wtStatus.m[k] = v
	}
	a.wtStatus.mu.Unlock()
	c = check()
	fresh := 0
	for _, w := range c.Worktrees {
		switch w.Status {
		case proto.WorktreeStatusFresh:
			fresh++
		case proto.WorktreeStatusCached:
		default:
			t.Fatalf("unexpected %+v", w)
		}
	}
	if st := statusExecs(logs()); fresh != 3 || st != 3+1 {
		t.Fatalf("refresh: %d fresh, %d status execs", fresh, st)
	}
	// Relocation's inspection is exact: no cache, no limit, no status marker.
	g, err := newProjectGit()
	if err != nil {
		t.Fatal(err)
	}
	before := statusExecs(logs())
	rc := g.inspectRepo(ctx, space, "repos/api")
	if got := statusExecs(logs()) - before; got != n+1 {
		t.Fatalf("exact inspection ran %d status execs, want %d", got, n+1)
	}
	for _, w := range rc.Worktrees {
		if w.Status != "" || w.Error != "" {
			t.Fatalf("exact inspection: %+v", w)
		}
	}
}
