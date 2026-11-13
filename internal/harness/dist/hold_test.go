package dist

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// installAll publishes and installs versions.
func installAll(t *testing.T, fr *fakeRelease, m *Manager, versions ...string) {
	t.Helper()
	for _, v := range versions {
		fr.add(v, binary(v), nil)
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
}

// Review S4: GC never removes a version between Acquire's decision and the launch. The decision and the hold are taken
// together under GC's lock, so a GC right after Acquire decided keeps the version although nothing else protects it;
// once released, it may go.
func TestAcquireRacesGC(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Keep = 1 })
	installAll(t, fr, m, "2.1.1", "2.1.2")
	var removedAfterHold []string
	gcRan := false
	m.afterHold = func(Resolution) {
		removed, err := m.GC(context.Background(), nil, nil)
		if err != nil {
			t.Error(err)
		}
		removedAfterHold, gcRan = removed, true
	}
	hits := fr.binaryHits.Load()
	res, release, err := m.Acquire(context.Background(), "2.1.1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !gcRan || len(removedAfterHold) != 0 {
		t.Fatalf("GC right after the decision removed %v (ran %v), want nothing", removedAfterHold, gcRan)
	}
	if fr.binaryHits.Load() != hits {
		t.Fatal("Acquire downloaded a cached version")
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, binary("2.1.1")) { //nolint:gosec // test cache
		t.Fatalf("%s is missing or wrong after Acquire", res.Path)
	}
	if !slices.Equal(m.Held(), []string{"2.1.1"}) {
		t.Fatalf("held %v", m.Held())
	}
	release()
	release() // idempotent
	if len(m.Held()) != 0 {
		t.Fatalf("held %v after release", m.Held())
	}
	if removed, _ := m.GC(context.Background(), nil, nil); !slices.Equal(removed, []string{"2.1.1"}) {
		t.Fatalf("GC after release removed %v, want 2.1.1", removed)
	}
	assertNoLeftovers(t, m.Dir())
}

// A version Acquire installs is held while it downloads, so a GC then can't remove it before the launch picks it.
func TestAcquireHoldsDownload(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Keep = 1 })
	installAll(t, fr, m, "2.1.5")
	fr.add("2.1.1", binary("2.1.1"), nil)
	started, proceed := make(chan struct{}), make(chan struct{})
	fr.setBinaryHook(func(w http.ResponseWriter, _ *http.Request, body []byte) {
		close(started)
		<-proceed
		_, _ = w.Write(body)
	})
	done := make(chan error, 1)
	go func() {
		_, release, err := m.Acquire(context.Background(), "2.1.1")
		if err == nil {
			release()
		}
		done <- err
	}()
	<-started
	if !slices.Equal(m.Held(), []string{"2.1.1"}) {
		t.Errorf("held while downloading: %v", m.Held())
	}
	close(proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(m.Held()) != 0 {
		t.Fatalf("held %v after release", m.Held())
	}
}

// GC calls inUse under the lock Acquire holds under: a launch that records its version (where inUse reads it) before
// releasing its hold is protected at every point.
func TestAcquireStressGC(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Keep = 1 })
	installAll(t, fr, m, "2.1.1", "2.1.2", "2.1.3")
	var mu sync.Mutex
	recorded := map[string]int{} // the "journal"
	used := func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		var out []string
		for v, n := range recorded {
			if n > 0 {
				out = append(out, v)
			}
		}
		return out, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := m.GC(ctx, nil, used); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for i := range 60 {
		v := []string{"2.1.1", "2.1.2", "2.1.3"}[i%3]
		res, release, err := m.Acquire(ctx, v)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		recorded[v]++
		mu.Unlock()
		release()
		// The "run" executes after the hold is gone: the record keeps the binary.
		if _, err := os.Stat(res.Path); err != nil {
			t.Fatalf("launch %d: %s removed while recorded: %v", i, v, err)
		}
		mu.Lock()
		recorded[v]--
		mu.Unlock()
	}
	close(stop)
	wg.Wait()
}

// A GC whose inUse fails removes nothing.
func TestGCInUseError(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Keep = 1 })
	installAll(t, fr, m, "2.1.1", "2.1.2")
	boom := errors.New("journal unavailable")
	removed, err := m.GC(context.Background(), nil, func(context.Context) ([]string, error) { return nil, boom })
	if !errors.Is(err, boom) || len(removed) != 0 {
		t.Fatalf("GC = %v, %v", removed, err)
	}
	if len(m.Installed()) != 2 {
		t.Fatal("GC removed a version without knowing what is in use")
	}
}

// Review S2, the tamper policy: a launch re-hashes only a binary whose stat fingerprint changed since this manager last
// hashed it; a changed binary that fails is removed and installed again, and one that still matches runs.
func TestAcquireTamperCheck(t *testing.T) {
	fr := newFakeRelease(t)
	dir := t.TempDir()
	m := newManager(t, fr, dir, nil)
	installAll(t, fr, m, "2.1.1")
	acquire := func() Resolution {
		t.Helper()
		res, release, err := m.Acquire(context.Background(), "2.1.1")
		if err != nil {
			t.Fatal(err)
		}
		release()
		return res
	}
	// Installed by this manager: no hash at launch.
	res := acquire()
	if n := m.hashes.Load(); n != 0 {
		t.Fatalf("%d hashes at a launch of a freshly installed binary", n)
	}
	// A metadata change (chmod back and forth) changes ctime: hashed again, still good, kept.
	if err := os.Chmod(res.Path, 0o555); err != nil { //nolint:gosec // the cached binary's own mode
		t.Fatal(err)
	}
	hits := fr.binaryHits.Load()
	acquire()
	if m.hashes.Load() != 1 || fr.binaryHits.Load() != hits {
		t.Fatalf("after a chmod: %d hashes, %d downloads; want 1 hash and no download", m.hashes.Load(), fr.binaryHits.Load()-hits)
	}
	acquire()
	if m.hashes.Load() != 1 {
		t.Fatal("hashed again although nothing changed")
	}
	// Replaced content of the same size: hashed, fails, removed and installed again.
	vdir := filepath.Dir(res.Path)
	if err := os.Chmod(vdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(res.Path, 0o755); err != nil { //nolint:gosec // test tampering
		t.Fatal(err)
	}
	evil := bytes.Repeat([]byte("e"), len(binary("2.1.1")))
	if err := os.WriteFile(res.Path, evil, 0o755); err != nil { //nolint:gosec // test tampering
		t.Fatal(err)
	}
	_ = os.Chmod(res.Path, 0o555) //nolint:gosec // as the cache leaves it
	_ = os.Chmod(vdir, 0o500)
	res = acquire()
	if fr.binaryHits.Load() != hits+1 {
		t.Fatal("a tampered binary was not installed again")
	}
	if got, _ := os.ReadFile(res.Path); !bytes.Equal(got, binary("2.1.1")) { //nolint:gosec // test cache
		t.Fatal("Acquire returned a tampered binary")
	}
	// A new manager (agent restart) knows no fingerprint: the first launch hashes once.
	m2 := newManager(t, fr, dir, nil)
	if _, release, err := m2.Acquire(context.Background(), "2.1.1"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if m2.hashes.Load() != 1 {
		t.Fatalf("restarted manager hashed %d times, want 1", m2.hashes.Load())
	}
	// Verify at startup records the fingerprint, so the launch after it doesn't hash.
	m3 := newManager(t, fr, dir, nil)
	if problems := m3.Verify(context.Background(), "2.1.1"); len(problems) != 0 {
		t.Fatal(problems)
	}
	if _, release, err := m3.Acquire(context.Background(), "2.1.1"); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if m3.hashes.Load() != 1 {
		t.Fatalf("launch after Verify hashed again (%d)", m3.hashes.Load())
	}
}

// A version that cannot be made usable fails Acquire after a bounded number of attempts.
func TestAcquireGivesUp(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	installAll(t, fr, m, "2.1.1")
	// Something other than GC (another agent on the directory, a person) removes it after every decision.
	m.afterHold = func(res Resolution) { _ = m.removeLocked(context.Background(), res.Version) }
	_, _, err := m.Acquire(context.Background(), "2.1.1")
	if err == nil {
		t.Fatal("Acquire succeeded although the version vanished every time")
	}
	if len(m.Held()) != 0 {
		t.Fatalf("held %v after a failed Acquire", m.Held())
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.afterHold = func(res Resolution) { cancel(); _ = m.removeLocked(context.Background(), res.Version) }
	if _, _, err := m.Acquire(ctx, "2.1.1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire with a cancelled context: %v", err)
	}
}

// Review S2 (PR #42): Verify removes a version only when it really failed verification. A cancelled or expired context
// ends the hash early, which says nothing about the binary: the version stays installed (and is hashed again later).
func TestVerifyCancelledContextKeepsVersion(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	installAll(t, fr, m, "2.1.1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	errs := m.Verify(ctx, "2.1.1")
	if errs["2.1.1"] == nil || errors.Is(errs["2.1.1"], ErrVerify) || !errors.Is(errs["2.1.1"], context.Canceled) {
		t.Fatalf("Verify on a cancelled context = %v, want context.Canceled, not ErrVerify", errs)
	}
	if _, ok := m.lookup("2.1.1"); !ok {
		t.Fatal("Verify on a cancelled context removed a good version")
	}
	if errs := m.Verify(context.Background(), "2.1.1"); len(errs) != 0 {
		t.Fatalf("Verify after = %v", errs)
	}
}

// Review S2: a launch whose acquire context has expired while it re-hashes the binary (its fingerprint is unknown)
// fails, but the version it checked stays installed.
func TestAcquireExpiredContextKeepsVersion(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	installAll(t, fr, m, "2.1.1")
	m.forgetHashed("2.1.1") // as after a restart: the launch must hash it
	ctx, cancel := context.WithCancel(context.Background())
	m.afterHold = func(Resolution) { cancel() } // the deadline passes between the hold and the launch check
	if _, _, err := m.Acquire(ctx, "2.1.1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want context.Canceled", err)
	}
	if err := m.launchCheck(ctx, "2.1.1"); err == nil {
		t.Fatal("launchCheck on an expired context passed without hashing")
	}
	if _, ok := m.lookup("2.1.1"); !ok {
		t.Fatal("an expired launch removed a good version")
	}
	if len(m.Held()) != 0 {
		t.Fatalf("held %v after a failed Acquire", m.Held())
	}
	m.afterHold = nil
	res, release, err := m.Acquire(context.Background(), "2.1.1")
	if err != nil || res.Version != "2.1.1" {
		t.Fatalf("Acquire after = %+v, %v", res, err)
	}
	release()
	if n := fr.binaryHits.Load(); n != 1 {
		t.Fatalf("binary downloaded %d times, want 1", n)
	}
}

// HoldCached holds a cached version without any network request and never waits: an uncached version is ErrNotCached
// at once (no download started), a cached one is held until released, and a damaged one fails its launch check.
func TestHoldCached(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	installAll(t, fr, m, "2.1.1")
	fr.add("2.1.2", binary("2.1.2"), nil)
	hits := fr.binaryHits.Load()
	if _, err := m.HoldCached(context.Background(), "2.1.2"); !errors.Is(err, ErrNotCached) {
		t.Fatalf("HoldCached of an uncached version = %v, want ErrNotCached", err)
	}
	if _, err := m.HoldCached(context.Background(), "latest"); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("HoldCached of a channel = %v", err)
	}
	release, err := m.HoldCached(context.Background(), "2.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Held(), []string{"2.1.1"}) {
		t.Fatalf("held %v", m.Held())
	}
	release()
	release()
	if len(m.Held()) != 0 {
		t.Fatalf("held %v after release", m.Held())
	}
	if fr.binaryHits.Load() != hits {
		t.Fatal("HoldCached downloaded")
	}
	// Same size, other content, and a changed fingerprint: the launch check re-hashes and removes it.
	p := filepath.Join(chmodW(t, m, "2.1.1"), BinaryName)
	b, _ := os.ReadFile(p) //nolint:gosec // test cache
	b[len(b)-1] ^= 1
	if err := os.WriteFile(p, b, 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	if _, err := m.HoldCached(context.Background(), "2.1.1"); !errors.Is(err, ErrVerify) {
		t.Fatalf("HoldCached of a damaged version = %v, want ErrVerify", err)
	}
	if _, ok := m.lookup("2.1.1"); ok || len(m.Held()) != 0 {
		t.Fatalf("damaged version kept (held %v)", m.Held())
	}
}
