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
