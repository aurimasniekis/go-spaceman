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

// WL-6 (worktree lifecycle): a repository report's git status execs are bounded. Worktrees idle for over a week reuse
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

// statusFixture is a space with one repository and n aged linked worktrees; check runs one report.
func statusFixture(t *testing.T, n, limit int) (a *Agent, space string, wts []string, check func() proto.RepoCheck) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	space, bare, _ := reposFixture(t)
	a = testAgent()
	oldMax := repoStatusMax
	repoStatusMax = limit
	t.Cleanup(func() { repoStatusMax = oldMax })
	api := proto.RepoSource{ID: "rep_api", Name: "api", Path: "repos/api", URL: bare, DefaultBranch: "main", ClonePolicy: proto.CloneAuto}
	p := proto.ReposSyncPayload{Repositories: []proto.RepoSource{api}, Clone: []string{"rep_api"}}
	check = func() proto.RepoCheck {
		rep := a.syncRepos(context.Background(), "spc_1", space, p)
		p.Clone = nil
		return *rep.Repositories[0].Check
	}
	check()
	repo := filepath.Join(space, "repos", "api")
	for i := range n {
		wt := filepath.Join(space, "worktrees", fmt.Sprintf("w%d", i))
		gitRun(t, repo, "worktree", "add", "-q", "-b", fmt.Sprintf("b%d", i), wt)
		age(t, wt, time.Now().Add(-10*24*time.Hour))
		wts = append(wts, wt)
	}
	return a, space, wts, check
}

func wtByName(c proto.RepoCheck, name string) proto.WorktreeCheck {
	for _, w := range c.Worktrees {
		if strings.HasSuffix(w.Path, "/"+name) {
			return w
		}
	}
	return proto.WorktreeCheck{}
}

// The cache key is the activity stamp and HEAD: a different stamp that is still old, or a different HEAD under the old
// stamp, must be checked again, not served from the cache.
func TestRepoReportWorktreeStatusCacheKey(t *testing.T) {
	_, _, wts, check := statusFixture(t, 2, 50)
	check() // fills the cache
	if c := check(); wtByName(c, "w0").Status != proto.WorktreeStatusCached || wtByName(c, "w1").Status != proto.WorktreeStatusCached {
		t.Fatalf("not cached: %+v", c.Worktrees)
	}
	logs := execLog(t)
	// Stamp: still older than a week, but not the one the counts were taken against.
	age(t, wts[0], time.Now().Add(-12*24*time.Hour))
	// HEAD: a new commit, the stamp put back to the old one.
	before, _ := worktreeStamp(wts[1])
	gitRun(t, wts[1], "commit", "-q", "--allow-empty", "-m", "x")
	age(t, wts[1], before)
	if got, _ := worktreeStamp(wts[1]); !got.Equal(before) {
		t.Fatalf("stamp moved: %v != %v", got, before)
	}
	c := check()
	for _, n := range []string{"w0", "w1"} {
		if w := wtByName(c, n); w.Status != proto.WorktreeStatusFresh {
			t.Fatalf("%s: %+v, want fresh (checked again)", n, w)
		}
	}
	if st := statusExecs(logs()); st != 3 { // two worktrees and the repository's own
		t.Fatalf("%d status execs, want 3", st)
	}
}

// A failed refresh keeps the cached counts and label; once they are past the maximum age they become unchecked.
func TestRepoReportWorktreeStatusFailedRefresh(t *testing.T) {
	a, _, wts, check := statusFixture(t, 1, 50)
	check()
	check()
	gd := strings.TrimPrefix(strings.TrimSpace(readText(t, filepath.Join(wts[0], ".git"))), "gitdir: ")
	// Make git status fail without changing the activity stamp the counts are cached against.
	stamp, _ := worktreeStamp(wts[0])
	idx := filepath.Join(gd, "index")
	if err := os.WriteFile(idx, []byte("not an index"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(idx, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got, _ := worktreeStamp(wts[0]); !got.Equal(stamp) {
		t.Fatalf("stamp moved: %v != %v", got, stamp)
	}
	setAt := func(d time.Duration) {
		a.wtStatus.mu.Lock()
		defer a.wtStatus.mu.Unlock()
		for k, v := range a.wtStatus.m {
			v.at = time.Now().Add(-d)
			a.wtStatus.m[k] = v
		}
	}
	setAt(2 * repoStatusRefresh)
	w := wtByName(check(), "w0")
	if w.Status != proto.WorktreeStatusCached || w.Error != "" || w.StatusAt == nil {
		t.Fatalf("a failed refresh must keep the cached counts: %+v", w)
	}
	if got := a.wtStatus.m[filepath.Join(wts[0])]; got.dirty != 0 || time.Since(got.at) < repoStatusRefresh {
		t.Fatalf("a failed refresh overwrote the cache: %+v", got)
	}
	setAt(repoStatusMaxAge + time.Hour)
	w = wtByName(check(), "w0")
	if w.Status != proto.WorktreeStatusUnchecked || w.Error == "" || w.Dirty != 0 || w.Untracked != 0 || w.StatusAt != nil {
		t.Fatalf("past the maximum age and unrefreshable: %+v", w)
	}
}

// Cached counts past the maximum age are refreshed first (oldest first) and, when the allowance runs out, reported
// unchecked instead of being kept.
func TestRepoReportWorktreeStatusMaxAge(t *testing.T) {
	a, _, _, check := statusFixture(t, 4, 2)
	for range 3 {
		check()
	}
	a.wtStatus.mu.Lock()
	i := 0
	for k, v := range a.wtStatus.m {
		v.at = time.Now().Add(-repoStatusMaxAge - time.Duration(i+1)*time.Hour)
		a.wtStatus.m[k] = v
		i++
	}
	a.wtStatus.mu.Unlock()
	m := map[string]int{}
	for _, w := range check().Worktrees {
		m[w.Status]++
		if w.Status == proto.WorktreeStatusUnchecked && (w.Error == "" || w.Dirty != 0 || w.Untracked != 0) {
			t.Fatalf("unchecked must carry an error and no counts: %+v", w)
		}
	}
	if m[proto.WorktreeStatusFresh] != 2 || m[proto.WorktreeStatusUnchecked] != 2 || m[proto.WorktreeStatusCached] != 0 {
		t.Fatalf("%v", m)
	}
}

// With more recently changed worktrees than a report may check, the unchecked ones differ from report to report.
func TestRepoReportWorktreeStatusRotates(t *testing.T) {
	_, _, wts, check := statusFixture(t, 6, 3)
	for _, wt := range wts { // recent again
		if err := os.WriteFile(filepath.Join(wt, "r.txt"), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for range 2 {
		for _, w := range check().Worktrees {
			if w.Status == proto.WorktreeStatusFresh {
				seen[w.Path] = true
			}
		}
	}
	if len(seen) != 6 {
		t.Fatalf("two reports checked %d of 6 recent worktrees, want all", len(seen))
	}
}

func TestRotate(t *testing.T) {
	if got := rotate([]int{0, 1, 2, 3, 4}, 1, 2); fmt.Sprint(got) != "[2 3 4 0 1]" {
		t.Fatalf("%v", got)
	}
	if got := rotate([]int{0, 1, 2}, 5, 0); fmt.Sprint(got) != "[0 1 2]" {
		t.Fatalf("%v", got)
	}
}
