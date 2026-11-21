package agent

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// localCommit commits everything in an instance checkout without publishing it.
func localCommit(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // test
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// An instance that committed locally while another instance published lacks the new shared head object, so counting
// head..HEAD fails. That must never read as clean: the flush has to publish the commit, and until then the space is
// not clean (a backup or relocation would lose it).
func TestMemoryStatusCountErrorIsNotClean(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	col := statusFleet(t, m, 1, 2)[0]
	sp, role := col[0], col[1]
	stopSyncs(m)
	i0, i1 := m.instance(sp, role, "ins_0"), m.instance(sp, role, "ins_1")

	write(t, i1, "local.md", "- unpublished\n")
	localCommit(t, i1, "local")
	write(t, i0, "published.md", "- published\n")
	if err := m.checkpointErr(ctx, sp, role, "ins_0", "test"); err != nil {
		t.Fatal(err)
	}
	stopSyncs(m)
	// Central has acknowledged the published head, so only the instance's own commit can make the space unclean.
	js, _ := a.j.memory(ctx, sp, role)
	js.Acked = m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if err := a.j.putMemory(ctx, js); err != nil {
		t.Fatal(err)
	}

	if ok, why := a.memoryClean(ctx, sp); ok {
		t.Fatalf("an instance with an unpublished commit read as clean (%s)", why)
	}
	a.flushSpaceMemory(ctx, sp, "backup")
	out, err := m.git.Run(ctx, m.shared(sp, role), nil, "show", "refs/heads/main:local.md")
	if err != nil || out != "- unpublished\n" {
		t.Fatalf("local.md did not reach shared main: %q %v", out, err)
	}
}

// A same-size rewrite that keeps the modification time (cp -p, rsync -a) changes the file's change time, so the
// fingerprint, and with it the cached status, must change.
func TestStatusStampSeesSameSizeRewriteWithKeptMtime(t *testing.T) {
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	dir := t.TempDir()
	p := filepath.Join(dir, "a.md")
	mt := time.Now().Add(-time.Hour)
	if err := os.WriteFile(p, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	before, ok := statusStamp(dir, "h")
	if !ok {
		t.Fatal("not cacheable")
	}
	time.Sleep(20 * time.Millisecond)
	if err := os.WriteFile(p, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	if after, _ := statusStamp(dir, "h"); after == before {
		t.Fatal("a same-size rewrite with a kept mtime left the fingerprint unchanged")
	}
}

// The cached status of removed or renamed collections and instances is dropped.
func TestStatusCachePrunesRemovedCollectionsAndInstances(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	cols := statusFleet(t, m, 2, 2)
	stopSyncs(m)
	// Work tree files older than the index, or git rewrites the index on every status (racy entries) and nothing caches.
	past := time.Now().Add(-time.Hour)
	for _, k := range cols {
		for _, i := range []string{"ins_0", "ins_1"} {
			root := m.instance(k[0], k[1], i)
			_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err == nil && !d.IsDir() && !strings.Contains(p, string(filepath.Separator)+".git"+string(filepath.Separator)) {
					_ = os.Chtimes(p, past, past)
				}
				return nil
			})
		}
	}
	for range 3 { // the first git status may rewrite an index, which is not cached
		for _, k := range cols {
			m.statusOf(ctx, k[0], k[1])
		}
	}
	cached := func() int {
		m.statusMu.Lock()
		defer m.statusMu.Unlock()
		return len(m.instStatus)
	}
	if n := cached(); n != 4 {
		t.Fatalf("cached before: %d", n)
	}
	// A collection is renamed away: a full report's pruning forgets its instances.
	if err := os.Rename(filepath.Join(m.root, cols[1][0], cols[1][1]), filepath.Join(m.root, cols[1][0], "renamed")); err != nil {
		t.Fatal(err)
	}
	m.pruneStatus([][2]string{cols[0], {cols[1][0], "renamed"}})
	if n := cached(); n != 2 {
		t.Fatalf("cached after the rename: %d", n)
	}
	// A removed instance is forgotten by its collection's next status.
	if err := os.RemoveAll(m.instance(cols[0][0], cols[0][1], "ins_1")); err != nil {
		t.Fatal(err)
	}
	m.statusOf(ctx, cols[0][0], cols[0][1])
	if n := cached(); n != 1 {
		t.Fatalf("cached after the removal: %d", n)
	}
	// A collection that is gone: the dirty-collection path forgets it.
	m.forgetStatus(filepath.Join(m.root, cols[0][0], cols[0][1], "instances"), nil)
	if n := cached(); n != 0 {
		t.Fatalf("cached after the collection went: %d", n)
	}
}

// unbornInstance replaces an instance with a fresh `git init` checkout: the state a failed or cancelled ensure leaves.
func unbornInstance(t *testing.T, m *memManager, sp, role, inst string) string {
	t.Helper()
	dir := m.instance(sp, role, inst)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // test
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", "-b", "main") //nolint:gosec // test
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return dir
}

// An unborn HEAD has no commits to publish: it counts 0 local commits (cached like any other checkout), so an empty
// one is clean, the space reads clean after a flush, and a relocation flush succeeds.
func TestMemoryStatusUnbornHeadIsClean(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	col := statusFleet(t, m, 1, 1)[0]
	sp, role := col[0], col[1]
	stopSyncs(m)
	unbornInstance(t, m, sp, role, "ins_0")
	js, _ := a.j.memory(ctx, sp, role)
	js.Acked = m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if err := a.j.putMemory(ctx, js); err != nil {
		t.Fatal(err)
	}
	a.flushSpaceMemory(ctx, sp, "backup")
	stopSyncs(m)
	if ok, why := a.memoryClean(ctx, sp); !ok {
		t.Fatalf("an empty unborn checkout read as unclean: %s", why)
	}
	count := countGit(t, m)
	inst, head := m.instance(sp, role, "ins_0"), m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	for range 2 {
		if _, n, ok := m.instanceStatus(ctx, inst, head); !ok || n != 0 {
			t.Fatalf("unborn checkout: ok=%v localCommits=%d, want 0", ok, n)
		}
	}
	c := count()
	if _, _, _ = m.instanceStatus(ctx, inst, head); count() != c {
		t.Fatalf("an unchanged unborn checkout was not cached: %d execs", count()-c)
	}
	if !a.relocFlush(ctx, sp, 10*time.Second) {
		t.Fatal("relocFlush failed for an unborn checkout")
	}
}

// Files written into an unborn checkout still count: it is dirty until they are checkpointed.
func TestMemoryStatusUnbornHeadWithFileIsDirty(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	col := statusFleet(t, m, 1, 1)[0]
	sp, role := col[0], col[1]
	stopSyncs(m)
	dir := unbornInstance(t, m, sp, role, "ins_0")
	write(t, dir, "note.md", "- written\n")
	if ok, _ := a.memoryClean(ctx, sp); ok {
		t.Fatal("an unborn checkout with an untracked file read as clean")
	}
	a.flushSpaceMemory(ctx, sp, "backup")
	stopSyncs(m)
	head := m.git.RevParse(ctx, m.shared(sp, role), "refs/heads/main")
	if n, _, ok := m.instanceStatus(ctx, dir, head); !ok || n != 0 {
		t.Fatalf("the file was not checkpointed: uncheckpointed=%d ok=%v", n, ok)
	}
}

// blockRevList points the memory manager's git at a wrapper that, the first time rev-list runs in dir, marks that it
// started and then blocks until it is killed. It returns the marker's path.
func blockRevList(t *testing.T, m *memManager, dir string) string {
	t.Helper()
	tmp := t.TempDir()
	started, once := filepath.Join(tmp, "started"), filepath.Join(tmp, "once")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" rev-list \"*)\n" +
		"  if [ \"$PWD\" = '" + dir + "' ] && mkdir '" + once + "' 2>/dev/null; then\n" +
		"    : > '" + started + "'\n" +
		"    while :; do sleep 0.05; done\n" +
		"  fi;;\n" +
		"esac\n" +
		"exec '" + m.git.Bin + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(tmp, "git"), []byte(script), 0o700); err != nil { //nolint:gosec // test wrapper
		t.Fatal(err)
	}
	m.git.Bin = filepath.Join(tmp, "git")
	return started
}

// A status whose context ends while it counts an instance's commits (a connection drop during a memory.flush) knows
// nothing about them. It must not read that as an unborn HEAD or cache it: a later status with a live context still
// sees the unpublished commit, the space is not clean, and the flush publishes it.
func TestMemoryStatusCancelledCountIsNotCachedAsClean(t *testing.T) {
	a := memAgent(t)
	ctx := context.Background()
	m := a.mem
	old := statusRacy
	statusRacy = 0
	t.Cleanup(func() { statusRacy = old })
	col := statusFleet(t, m, 1, 2)[0]
	sp, role := col[0], col[1]
	stopSyncs(m)
	inst := m.instance(sp, role, "ins_1")
	write(t, inst, "local.md", "- unpublished\n")
	localCommit(t, inst, "local")
	shared := m.shared(sp, role)
	head := m.git.RevParse(ctx, shared, "refs/heads/main")
	js, _ := a.j.memory(ctx, sp, role)
	js.Acked = head
	if err := a.j.putMemory(ctx, js); err != nil {
		t.Fatal(err)
	}
	// Work tree files older than the index, and the index settled (the first git status may rewrite it), so that the
	// cancelled status below could be cached; then drop the cached entry: the cancelled status has to count.
	past := time.Now().Add(-time.Hour)
	_ = filepath.WalkDir(inst, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.Contains(p, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			_ = os.Chtimes(p, past, past)
		}
		return nil
	})
	for range 3 {
		if _, n, ok := m.instanceStatus(ctx, inst, head); !ok || n != 1 {
			t.Fatalf("before: ok=%v localCommits=%d, want 1", ok, n)
		}
	}
	m.statusMu.Lock()
	_, primed := m.instStatus[inst]
	delete(m.instStatus, inst)
	m.statusMu.Unlock()
	if !primed {
		t.Fatal("the settled checkout's status was not cached, so the probe proves nothing")
	}

	bin := m.git.Bin
	started := blockRevList(t, m, inst)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for !exists(started) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.instanceStatus(cctx, inst, head)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the status did not end after its context was cancelled")
	}
	if !exists(started) {
		t.Fatal("the status never counted the instance's commits")
	}
	m.git.Bin = bin

	m.statusMu.Lock()
	c, cached := m.instStatus[inst]
	m.statusMu.Unlock()
	if cached {
		t.Errorf("a status under a cancelled context was cached: %+v", c)
	}
	if _, n, ok := m.instanceStatus(ctx, inst, head); !ok || n < 1 {
		t.Errorf("after the cancelled status: ok=%v localCommits=%d, want at least 1", ok, n)
	}
	if ok, why := a.memoryClean(ctx, sp); ok {
		t.Errorf("an instance with an unpublished commit read as clean (%s)", why)
	}
	a.flushSpaceMemory(ctx, sp, "backup")
	out, err := m.git.Run(ctx, shared, nil, "show", "refs/heads/main:local.md")
	if err != nil || out != "- unpublished\n" {
		t.Fatalf("local.md did not reach shared main: %q %v", out, err)
	}
}
