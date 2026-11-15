package dist

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Installed lists the verified cached versions, newest first.
func (m *Manager) Installed() []Installed {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil
	}
	var out []Installed
	for _, e := range entries {
		if !ValidVersion(e.Name()) {
			continue // lookup alone decides what is installed, so Installed and Ensure agree
		}
		if inst, ok := m.lookup(e.Name()); ok {
			out = append(out, inst)
		}
	}
	slices.SortFunc(out, func(a, b Installed) int { return CompareVersions(b.Version, a.Version) })
	return out
}

// GC removes cached versions beyond the newest Keep, except protected ones: the versions pinned by profiles (supplied
// by the caller), the versions inUse returns (the caller's live and adoptable runs), the versions held by an Acquire
// that has not been released, and the memoised channel targets and their previous targets (the fallbacks). A version
// another process is installing or removing is skipped. It returns the removed versions.
//
// GC runs under the lock Acquire registers its hold under, and calls inUse under it too, so a version is never removed
// between a launch resolving it and the launch being recorded where inUse reads it (nil: none in use).
func (m *Manager) GC(ctx context.Context, pinned []string, inUse func(context.Context) ([]string, error)) ([]string, error) {
	m.gcMu.Lock()
	defer m.gcMu.Unlock()
	keep := map[string]bool{}
	for _, v := range pinned {
		keep[v] = true
	}
	if inUse != nil {
		used, err := inUse(ctx)
		if err != nil {
			return nil, fmt.Errorf("claude code: versions in use: %w", err)
		}
		for _, v := range used {
			keep[v] = true
		}
	}
	for v := range m.held {
		keep[v] = true
	}
	for _, v := range m.Resolved() {
		keep[v] = true
	}
	for _, v := range m.previousTargets() {
		keep[v] = true
	}
	var removed []string
	for i, inst := range m.Installed() {
		if i < m.keep || keep[inst.Version] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		ok, err := m.removeIfUnlocked(inst.Version)
		if err != nil {
			return removed, err
		}
		if ok {
			removed = append(removed, inst.Version)
			m.log.Info("claude code version removed from the cache", slog.String("version", inst.Version))
		}
	}
	return removed, nil
}

// Clean removes leftovers of interrupted installs (<v>.tmp-*) and removals (<v>.gc-*). A temporary directory whose
// version another process is installing right now is left alone.
func (m *Manager) Clean() error {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if v, _, ok := strings.Cut(name, ".gc-"); ok && ValidVersion(v) {
			_ = removeTree(filepath.Join(m.dir, name))
			continue
		}
		v, _, ok := strings.Cut(name, ".tmp-")
		if !ok || !ValidVersion(v) {
			continue
		}
		unlock, locked, err := tryLockFile(m.lockPath(v))
		if err != nil || !locked {
			continue
		}
		_ = removeTree(filepath.Join(m.dir, name))
		unlock()
	}
	return nil
}

// removeIfUnlocked removes version unless another process holds its lock.
func (m *Manager) removeIfUnlocked(version string) (bool, error) {
	unlock, locked, err := tryLockFile(m.lockPath(version))
	if err != nil || !locked {
		return false, err
	}
	defer unlock()
	return true, m.remove(version)
}

// removeLocked waits for version's lock and removes it.
func (m *Manager) removeLocked(ctx context.Context, version string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	unlock, err := lockFile(ctx, m.lockPath(version))
	if err != nil {
		return err
	}
	defer unlock()
	return m.remove(version)
}

// remove renames the version directory (or whatever sits at its name) out of the way first, so a half-deleted
// directory never looks like a version, then deletes it. The caller holds the version's lock. Running processes keep
// the deleted binary's inode.
func (m *Manager) remove(version string) error {
	m.forgetHashed(version)
	gone := filepath.Join(m.dir, version+".gc-"+randSuffix())
	if err := os.Rename(m.versionDir(version), gone); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	m.notify() // renamed away: the version is no longer cached, whatever deleting its files does
	return removeTree(gone)
}

// removeTree deletes path. A real directory is made writable first: verified version directories are read-only
// (0500). A link is removed itself, never followed.
func removeTree(path string) error {
	if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
		if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // G302: our own directory, to delete it
			return err
		}
	}
	return os.RemoveAll(path)
}
