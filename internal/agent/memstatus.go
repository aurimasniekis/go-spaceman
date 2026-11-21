package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

// Memory status reporting (SM-MEM-004) is coalesced and cached. A status costs git in every instance checkout (status
// and rev-list, each behind the checkout guard's own git calls), so reporting every collection on every checkpoint,
// acknowledgement and failed sync made the node's git execs grow with the number of instance folders times the number
// of events. Instead:
//
//   - an event only marks its collection changed; one report goes out per statusInterval at most, holding the changed
//     collections (all of them after a reconnect), never inline on an event;
//   - an instance's counts are cached under a fingerprint of its files (names, sizes, modification times, the checkout's
//     index, HEAD and refs) and the shared head, so an unchanged checkout (an ended instance's, typically) costs no git.
//
// The counts central shows may so be up to one interval old.

// statusInterval is the shortest time between two status reports unless Options.MemoryStatusInterval sets one. A
// variable so tests can shorten it.
var statusInterval = 30 * time.Second

// statusRacy is how recent a file's modification may be for its checkout's counts to be cached: an edit within the
// file system's timestamp granularity could leave size and time unchanged (git's "racy" entries). A variable so tests
// can turn it off.
var statusRacy = 2 * time.Second

// instStatus is the cached status of one instance checkout.
type instStatus struct {
	stamp          uint64
	uncheckpointed int
	localCommits   int
}

// statusChanged marks a collection's status as changed; it is reported within statusInterval.
func (m *memManager) statusChanged(spaceID, role string) {
	m.markStatus(func() { m.statusDirty[colKey(spaceID, role)] = [2]string{spaceID, role} })
}

// statusChangedAll marks every collection on disk as changed (the node connected again).
func (m *memManager) statusChangedAll() {
	m.markStatus(func() { m.statusAll = true })
}

func (m *memManager) markStatus(mark func()) {
	if m.git == nil {
		return
	}
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	mark()
	if m.statusTimer != nil {
		return // a report is due: it takes this change too
	}
	every := statusInterval
	if m.a.o.MemoryStatusInterval > 0 {
		every = m.a.o.MemoryStatusInterval
	}
	delay := time.Until(m.statusLast.Add(every))
	m.statusTimer = time.AfterFunc(max(delay, 0), func() { m.a.track(func() { m.reportStatus(m.base) }) })
}

// reportStatus sends the status of the collections that changed since the last report. Reports never overlap.
func (m *memManager) reportStatus(ctx context.Context) {
	m.reportMu.Lock()
	defer m.reportMu.Unlock()
	m.statusMu.Lock()
	dirty, all := m.statusDirty, m.statusAll
	m.statusDirty, m.statusAll, m.statusTimer, m.statusLast = map[string][2]string{}, false, nil, time.Now()
	m.statusMu.Unlock()
	m.a.mu.Lock()
	ok := m.a.connected && m.a.minor >= proto.MinorMemory
	m.a.mu.Unlock()
	if !ok {
		return // reported in full again on reconnect (resume)
	}
	var cols [][2]string
	if all {
		cols = m.collections(ctx)
		m.pruneStatus(cols)
	} else {
		for _, k := range dirty {
			if exists(filepath.Join(m.shared(k[0], k[1]), "HEAD")) && !m.a.j.orphanedRole(ctx, k[0], k[1]) {
				cols = append(cols, k)
			} else {
				m.forgetStatus(filepath.Join(m.root, k[0], k[1], "instances"), nil) // removed or renamed
			}
		}
		if len(cols) == 0 {
			return
		}
	}
	st := proto.MemoryStatus{Collections: []proto.MemoryCollectionStatus{}}
	for _, k := range cols {
		st.Collections = append(st.Collections, m.statusOf(ctx, k[0], k[1]))
	}
	if m.statusSent != nil {
		m.statusSent(st)
	}
	_ = m.a.send(ctx, proto.TypeMemoryStatus, st)
}

// instanceStatus is an instance checkout's uncheckpointed changes and the commits it has not published, from the cache
// when nothing in the checkout changed. ok is false for a folder that is not a checkout of its own.
func (m *memManager) instanceStatus(ctx context.Context, inst, head string) (uncheckpointed, localCommits int, ok bool) {
	pre, cacheable := statusStamp(inst, head)
	if cacheable {
		m.statusMu.Lock()
		c, hit := m.instStatus[inst]
		m.statusMu.Unlock()
		if hit && c.stamp == pre {
			return c.uncheckpointed, c.localCommits, true
		}
	}
	out, err := m.git.RunCheckout(ctx, inst, nil, "status", "--porcelain")
	if err != nil {
		m.statusMu.Lock()
		delete(m.instStatus, inst)
		m.statusMu.Unlock()
		return 0, 0, false // not a checkout of its own: nothing is counted, and no git runs there
	}
	uncheckpointed = len(strings.Split(strings.TrimSpace(out), "\n")) - boolToInt(strings.TrimSpace(out) == "")
	// Commits an instance has not published (it diverged and is waiting for its next checkpoint).
	countFailed := false
	if head != "" {
		n, err := m.git.CountErr(ctx, inst, head, "HEAD")
		if err != nil {
			// An idle instance has not fetched the shared head yet: bring its objects in (what a checkpoint does first)
			// and count again.
			shared := filepath.Join(filepath.Dir(filepath.Dir(inst)), "shared.git")
			if _, ferr := m.git.RunCheckout(ctx, inst, nil, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", shared, "main"); ferr == nil {
				n, err = m.git.CountErr(ctx, inst, head, "HEAD")
			}
		}
		if err != nil && m.unbornHead(ctx, inst) {
			// An unborn HEAD (a checkout that was only initialised, as a failed or cancelled ensure leaves it) has no
			// commits to publish; its files still count through the status above. Cached as usual.
			n, err = 0, nil
		}
		if err != nil {
			// Typically the instance diverged and lacks the new shared head's objects. Never read that as clean: count
			// everything the checkout has (an upper bound, at least 1) so the flush checkpoints it and the space is
			// not clean until it is published. Not cached.
			countFailed = true
			n, err = m.git.CountErr(ctx, inst, "", "HEAD")
			if err != nil || n < 1 {
				n = 1
			}
		}
		localCommits = n
	}
	// Cached only when every step answered (an ended context or a failed count is never cached: what it read may be
	// wrong) and nothing changed while git ran (git status may rewrite the index: the next report counts again).
	if post, ok := statusStamp(inst, head); cacheable && ok && !countFailed && ctx.Err() == nil && post == pre {
		m.statusMu.Lock()
		m.instStatus[inst] = instStatus{stamp: pre, uncheckpointed: uncheckpointed, localCommits: localCommits}
		m.statusMu.Unlock()
	}
	return uncheckpointed, localCommits, true
}

// unbornHead reports whether a checkout's HEAD names a branch that does not exist yet (a checkout that was only
// initialised). Only positive evidence counts, never an error: the context must not have ended, git must have run and
// answered "no such commit" (rev-parse --verify exit 1), HEAD must be a symbolic ref to a branch, and that branch must be
// neither a loose nor a packed ref. Anything else (an ended context, a refused, killed or failed git, a detached HEAD, an
// unreadable ref store) is not unborn, so the caller keeps counting the checkout as unpublished.
func (m *memManager) unbornHead(ctx context.Context, inst string) bool {
	if ctx.Err() != nil {
		return false
	}
	_, err := m.git.RunCheckout(ctx, inst, nil, "rev-parse", "--verify", "-q", "HEAD")
	var ee *exec.ExitError
	if err == nil || !errors.As(err, &ee) || ee.ExitCode() != 1 || ctx.Err() != nil {
		return false
	}
	out, err := m.git.RunCheckout(ctx, inst, nil, "symbolic-ref", "-q", "HEAD")
	if err != nil || ctx.Err() != nil {
		return false
	}
	return branchAbsent(filepath.Join(inst, ".git"), strings.TrimSpace(out))
}

// branchAbsent reports whether ref (refs/heads/<b>) is neither a loose ref nor a packed-refs entry of the git dir. It
// is false when the ref store cannot be read or is not the files backend.
func branchAbsent(gitDir, ref string) bool {
	b, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || b == "" || !filepath.IsLocal(b) {
		return false
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "reftable")); !os.IsNotExist(err) {
		return false // another ref backend (or an unreadable folder): no evidence
	}
	if _, err := os.Lstat(filepath.Join(gitDir, "refs", "heads", filepath.FromSlash(b))); !os.IsNotExist(err) {
		return false
	}
	p := filepath.Join(gitDir, "packed-refs")
	fi, err := os.Lstat(p)
	switch {
	case os.IsNotExist(err):
		return true
	case err != nil || !fi.Mode().IsRegular():
		return false
	}
	data, err := os.ReadFile(p) //nolint:gosec // the checkout's own packed-refs, a regular file (checked above and by the guard)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[1] == ref {
			return false
		}
	}
	return true
}

// forgetStatus drops the cached status of a collection's instances that are no longer on disk.
func (m *memManager) forgetStatus(instances string, seen map[string]bool) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	for p := range m.instStatus {
		if filepath.Dir(p) == instances && !seen[p] {
			delete(m.instStatus, p)
		}
	}
}

// pruneStatus drops the cached status of every instance outside the given collections (renamed or removed ones).
func (m *memManager) pruneStatus(cols [][2]string) {
	keep := map[string]bool{}
	for _, k := range cols {
		keep[filepath.Join(m.root, k[0], k[1], "instances")] = true
	}
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	for p := range m.instStatus {
		if !keep[filepath.Dir(p)] {
			delete(m.instStatus, p)
		}
	}
}

// statusStamp fingerprints what an instance's status depends on: every work tree entry (path, type, size, modification
// time, inode, change time), the checkout's index, HEAD, refs and the configuration files that change what git ignores, and the shared
// head. It costs one lstat per file and no git. cacheable is false when the folder cannot be read or a file changed
// within statusRacy.
func statusStamp(inst, head string) (uint64, bool) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(head))
	racy := time.Now().Add(-statusRacy)
	recent := false
	var buf [8]byte
	add := func(rel string, fi fs.FileInfo) {
		_, _ = h.Write([]byte(rel))
		_, _ = h.Write([]byte{0})
		binary.LittleEndian.PutUint64(buf[:], uint64(fi.Mode()))
		_, _ = h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(fi.Size())) //nolint:gosec // a size is never negative
		_, _ = h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(fi.ModTime().UnixNano())) //nolint:gosec // only hashed
		_, _ = h.Write(buf[:])
		ino, ctime := fileIdentity(fi)
		binary.LittleEndian.PutUint64(buf[:], ino)
		_, _ = h.Write(buf[:])
		binary.LittleEndian.PutUint64(buf[:], uint64(ctime)) //nolint:gosec // only hashed
		_, _ = h.Write(buf[:])
		if fi.ModTime().After(racy) || time.Unix(0, ctime).After(racy) {
			recent = true
		}
	}
	walk := func(dir string, skipGit bool) error {
		return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(inst, p)
			if skipGit && rel == ".git" {
				return filepath.SkipDir
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			add(rel, fi)
			return nil
		})
	}
	if err := walk(inst, true); err != nil {
		return 0, false
	}
	git := filepath.Join(inst, ".git")
	for _, f := range []string{"HEAD", "index", "packed-refs", "config", filepath.Join("info", "exclude")} {
		fi, err := os.Lstat(filepath.Join(git, f))
		switch {
		case err == nil:
			add(filepath.Join(".git", f), fi)
		case !os.IsNotExist(err):
			return 0, false
		}
	}
	if err := walk(filepath.Join(git, "refs"), false); err != nil && !os.IsNotExist(err) {
		return 0, false
	}
	return h.Sum64(), !recent
}
