package dist

import (
	"context"
	"fmt"
	"io/fs"
	"sync"
)

// acquireAttempts bounds how often Acquire decides again after installing a version or after the version it held
// failed its launch check.
const acquireAttempts = 5

// fingerprint identifies a binary file as it was when this manager hashed it.
type fingerprint struct {
	dev, ino            uint64
	size                int64
	mode                fs.FileMode
	ctimeSec, ctimeNsec int64
}

// Acquire resolves requested as Resolve does and holds the resolved version until release is called: GC never removes
// a held version, so the binary a launch resolved is still there when the launch executes it. The decision from the
// cache and the hold are taken together under GC's lock (network requests and downloads happen outside it; a version
// being installed for the launch is held while it downloads), so no GC runs between them. The caller releases the hold
// once the run is recorded where GC's inUse reads it (the agent's journal). release may be called more than once.
//
// Acquire then checks the binary at launch (the tamper policy, ADR-0077): when its stat fingerprint (device, inode,
// size, mode and ctime) is the one recorded when this manager last hashed it (at install or Verify), it runs as is;
// otherwise it is hashed again (about 0.35 s for 340 MB) and, when that fails, removed and resolved again. This finds a
// binary changed before the check, not one replaced between the check (Acquire) and the exec by something running as
// the node's user; that gap includes whatever launch preparation the caller does after Acquire (the agent acquires
// right before it builds the launch with HoldCached, then writes the launch files and starts it). Runs share that uid, so the cache is
// no boundary against them. A check cut short by ctx keeps the version (Verify).
func (m *Manager) Acquire(ctx context.Context, requested string) (Resolution, func(), error) {
	requested, target, perr, err := m.target(ctx, requested)
	if err != nil {
		return Resolution{}, nil, err
	}
	var last error
	pending := func() {} // the hold on a version being installed for this launch
	defer func() { pending() }()
	for range acquireAttempts {
		m.gcMu.Lock()
		res, want, err := m.pick(requested, target, perr) //nolint:contextcheck // pick only starts background installs on the manager's context
		switch {
		case err != nil:
		case want != "":
			m.held[want]++
		default:
			m.held[res.Version]++
		}
		m.gcMu.Unlock()
		pending()
		pending = func() {}
		if err != nil {
			return Resolution{}, nil, err
		}
		if want != "" {
			pending = m.releaser(want)
			if _, err := m.Ensure(ctx, want); err != nil {
				return Resolution{}, nil, err
			}
			continue // picked again under the lock, now from the cache
		}
		release := m.releaser(res.Version)
		if m.afterHold != nil {
			m.afterHold(res)
		}
		err = m.launchCheck(ctx, res.Version)
		if err == nil {
			return res, release, nil
		}
		release()
		if cerr := ctx.Err(); cerr != nil {
			return Resolution{}, nil, cerr
		}
		last = err
	}
	if last == nil {
		last = ErrNoVersion
	}
	return Resolution{}, nil, fmt.Errorf("claude code %s: no usable version after %d attempts: %w", requested, acquireAttempts, last)
}

// HoldCached holds an exact version for a launch only if it is cached now: no network request, no download, no wait.
// The cache check and the hold are taken together under GC's lock, as in Acquire, then the version gets Acquire's launch
// check (a re-hash only when its fingerprint changed; a damaged version is removed). A version that is not cached, or
// that fails the check, is an error matching ErrNotCached or the check's error; the caller resolves again (Acquire).
// release may be called more than once.
func (m *Manager) HoldCached(ctx context.Context, version string) (func(), error) {
	if !ValidVersion(version) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidVersion, version)
	}
	m.gcMu.Lock()
	inst, ok := m.lookup(version)
	if ok {
		m.held[version]++
	}
	m.gcMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: claude code %s", ErrNotCached, version)
	}
	release := m.releaser(version)
	if m.afterHold != nil {
		m.afterHold(Resolution{Requested: version, Target: version, Version: version, Path: inst.Path})
	}
	if err := m.launchCheck(ctx, version); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// releaser returns the idempotent release of one hold on version.
func (m *Manager) releaser(version string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			m.gcMu.Lock()
			defer m.gcMu.Unlock()
			if m.held[version]--; m.held[version] <= 0 {
				delete(m.held, version)
			}
		})
	}
}

// Held returns the versions launches hold right now (tests, reporting).
func (m *Manager) Held() []string {
	m.gcMu.Lock()
	defer m.gcMu.Unlock()
	out := make([]string, 0, len(m.held))
	for v := range m.held {
		out = append(out, v)
	}
	return out
}

// launchCheck checks a held version before it runs: the cheap check, then a re-hash unless the binary's stat
// fingerprint is the one recorded when it was last hashed. A version that fails is removed (Verify).
func (m *Manager) launchCheck(ctx context.Context, version string) error {
	if _, err := m.check(version); err != nil {
		return err // removed meanwhile (GC before the hold), or damaged: Resolve installs it again
	}
	if fp, ok := m.binaryFingerprint(version); ok {
		m.mu.Lock()
		want, known := m.hashed[version]
		m.mu.Unlock()
		if known && fp == want {
			return nil
		}
	}
	if problems := m.Verify(ctx, version); problems[version] != nil {
		return problems[version]
	}
	if _, err := m.check(version); err != nil {
		return err
	}
	return nil
}

// binaryFingerprint stats version's binary through the version directory's handle (no link followed).
func (m *Manager) binaryFingerprint(version string) (fingerprint, bool) {
	vd, err := m.openVersion(version)
	if err != nil {
		return fingerprint{}, false
	}
	defer func() { _ = vd.Close() }()
	fi, err := vd.Lstat(BinaryName)
	if err != nil || !fi.Mode().IsRegular() {
		return fingerprint{}, false
	}
	return statFingerprint(fi)
}

// rememberHashed records the fingerprint of a binary this manager has just hashed (fp, ok from statFingerprint of the
// very file hashed).
func (m *Manager) rememberHashed(version string, fp fingerprint, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok {
		m.hashed[version] = fp
	} else {
		delete(m.hashed, version)
	}
}

// forgetHashed drops a version's fingerprint (it was removed).
func (m *Manager) forgetHashed(version string) {
	m.mu.Lock()
	delete(m.hashed, version)
	m.mu.Unlock()
}
