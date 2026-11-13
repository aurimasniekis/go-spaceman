package dist

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/proto"
)

const (
	testPlatform = "linux-x64"
	basePath     = "/claude-code-releases"
)

// fakeRelease is an httptest TLS stand-in for the release host: {base}/{channel}, {base}/{v}/manifest.json and
// {base}/{v}/{platform}/claude. The real host is never contacted.
type fakeRelease struct {
	srv *httptest.Server

	mu        sync.Mutex
	pointers  map[string]string
	manifests map[string][]byte
	binaries  map[string][]byte
	pointerOK bool
	// binaryHook, when set, serves binary requests instead of the default handler.
	binaryHook func(w http.ResponseWriter, r *http.Request, body []byte)

	pointerHits  atomic.Int32
	binaryHits   atomic.Int32
	manifestHits atomic.Int32
}

func newFakeRelease(t *testing.T) *fakeRelease {
	t.Helper()
	fr := &fakeRelease{pointers: map[string]string{}, manifests: map[string][]byte{}, binaries: map[string][]byte{}, pointerOK: true}
	fr.srv = httptest.NewUnstartedServer(http.HandlerFunc(fr.serve))
	fr.srv.Config.ErrorLog = log.New(io.Discard, "", 0) // handshakes cut short by a closing test
	fr.srv.StartTLS()
	t.Cleanup(fr.srv.Close)
	return fr
}

func (fr *fakeRelease) serve(w http.ResponseWriter, r *http.Request) {
	rest, ok := strings.CutPrefix(r.URL.Path, basePath+"/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(rest, "/")
	fr.mu.Lock()
	var (
		body   []byte
		found  bool
		status = http.StatusOK
		hook   func(http.ResponseWriter, *http.Request, []byte)
	)
	switch {
	case len(parts) == 1:
		fr.pointerHits.Add(1)
		var v string
		v, found = fr.pointers[parts[0]]
		body = []byte(v + "\n")
		if !fr.pointerOK {
			status = http.StatusServiceUnavailable
		}
	case len(parts) == 2 && parts[1] == "manifest.json":
		fr.manifestHits.Add(1)
		body, found = fr.manifests[parts[0]]
	case len(parts) == 3 && parts[1] == testPlatform && parts[2] == BinaryName:
		fr.binaryHits.Add(1)
		body, found = fr.binaries[parts[0]]
		hook = fr.binaryHook
	}
	fr.mu.Unlock()
	switch {
	case status != http.StatusOK:
		http.Error(w, "unavailable", status)
	case !found:
		http.NotFound(w, r)
	case hook != nil:
		hook(w, r, body)
	default:
		_, _ = w.Write(body)
	}
}

// add publishes version with body and a matching manifest; edit may change the manifest before it is encoded.
func (fr *fakeRelease) add(version string, body []byte, edit func(m map[string]any)) {
	sum := sha256.Sum256(body)
	m := map[string]any{
		"version":                      version,
		"manifestSignatureEnforcement": "flag",
		"platforms": map[string]any{
			testPlatform: map[string]any{"binary": BinaryName, "checksum": hex.EncodeToString(sum[:]), "size": len(body)},
			"darwin-arm64": map[string]any{
				"binary": BinaryName, "checksum": strings.Repeat("0", 64), "size": 1,
			},
		},
	}
	if edit != nil {
		edit(m)
	}
	raw, _ := json.Marshal(m)
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.manifests[version] = raw
	fr.binaries[version] = body
}

func (fr *fakeRelease) setPointer(channel, version string) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.pointers[channel] = version
}

func (fr *fakeRelease) setPointerOK(ok bool) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.pointerOK = ok
}

func (fr *fakeRelease) setBinaryHook(h func(http.ResponseWriter, *http.Request, []byte)) {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	fr.binaryHook = h
}

// inUse is a GC inUse callback returning versions.
func inUse(versions ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) { return versions, nil }
}

func binary(version string) []byte {
	return []byte("#!/bin/sh\necho " + version + "\n" + strings.Repeat("x", 4096))
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newManager(t *testing.T, fr *fakeRelease, dir string, edit func(*Options)) *Manager {
	t.Helper()
	o := Options{
		Dir: dir, BaseURL: fr.srv.URL + basePath, Platform: testPlatform, Client: fr.srv.Client(),
		Refresh: time.Hour, DownloadTimeout: 30 * time.Second,
	}
	if edit != nil {
		edit(&o)
	}
	t.Cleanup(func() { makeWritable(dir) }) // runs after m.Close, before the temp dir is removed
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

// makeWritable makes every directory under dir writable again, so the test's temp dir can be removed: verified
// version directories are read-only (0500).
func makeWritable(dir string) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o700) //nolint:gosec // test cleanup
		}
		return nil
	})
}

// assertNothingExecutable fails when dir holds an executable regular file outside a verified version directory.
func assertNothingExecutable(t *testing.T, m *Manager) {
	t.Helper()
	verified := map[string]bool{}
	for _, inst := range m.Installed() {
		verified[inst.Path] = true
	}
	_ = filepath.WalkDir(m.Dir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a file removed while walking is not executable
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err == nil && fi.Mode().Perm()&0o111 != 0 && !verified[p] {
			t.Errorf("%s is executable but not a verified install", p)
		}
		return nil
	})
}

func assertNoLeftovers(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") || strings.Contains(e.Name(), ".gc-") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnsureInstallsVerifiedImmutableVersion(t *testing.T) {
	fr := newFakeRelease(t)
	body := binary("2.1.322")
	fr.add("2.1.322", body, nil)
	m := newManager(t, fr, filepath.Join(t.TempDir(), "harness", "claude-code"), nil)

	inst, err := m.Ensure(context.Background(), "2.1.322")
	if err != nil {
		t.Fatal(err)
	}
	if inst.Path != filepath.Join(m.Dir(), "2.1.322", "claude") {
		t.Fatalf("path %s", inst.Path)
	}
	got, err := os.ReadFile(inst.Path)
	if err != nil || string(got) != string(body) {
		t.Fatalf("binary content: %v", err)
	}
	// Read-only once verified: binary 0555, metadata 0400, version directory 0500.
	for name, want := range map[string]fs.FileMode{
		"": 0o500, BinaryName: 0o555, verifiedFile: 0o400, manifestFile: 0o400,
	} {
		fi, err := os.Lstat(filepath.Join(m.Dir(), "2.1.322", name))
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%q mode %v (%v), want %v", name, fi.Mode().Perm(), err, want)
		}
	}
	if kept, _ := os.ReadFile(filepath.Join(m.Dir(), "2.1.322", manifestFile)); string(kept) != string(fr.manifests["2.1.322"]) {
		t.Fatal("manifest.json is not the release manifest")
	}
	di, _ := os.Stat(m.Dir())
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("cache dir mode %v, want 0700", di.Mode().Perm())
	}
	raw, err := os.ReadFile(filepath.Join(m.Dir(), "2.1.322", "verified.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v Installed
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	msum := sha256.Sum256(fr.manifests["2.1.322"])
	if v.Version != "2.1.322" || v.Platform != testPlatform || v.SHA256 != hex.EncodeToString(sum[:]) ||
		v.Size != int64(len(body)) || v.ManifestSHA256 != hex.EncodeToString(msum[:]) || v.FetchedAt.IsZero() {
		t.Fatalf("verified.json = %+v", v)
	}
	for _, k := range []string{"version", "platform", "sha256", "size", "manifest_sha256", "fetched_at"} {
		if !strings.Contains(string(raw), `"`+k+`"`) {
			t.Errorf("verified.json lacks %q", k)
		}
	}
	// Cached: no second download.
	if _, err := m.Ensure(context.Background(), "2.1.322"); err != nil {
		t.Fatal(err)
	}
	if n := fr.binaryHits.Load(); n != 1 {
		t.Fatalf("binary downloaded %d times, want 1", n)
	}
	if errs := m.Verify(context.Background(), "2.1.322", "9.9.9"); len(errs) != 0 {
		t.Fatalf("Verify: %v", errs)
	}
	assertNoLeftovers(t, m.Dir())
}

func TestDownloadVerificationFailures(t *testing.T) {
	body := binary("2.1.5")
	other := sha256.Sum256([]byte("something else"))
	cases := []struct {
		name string
		edit func(map[string]any)
		hook func(http.ResponseWriter, *http.Request, []byte)
		want error
	}{
		{name: "bad checksum", edit: func(m map[string]any) {
			m["platforms"].(map[string]any)[testPlatform].(map[string]any)["checksum"] = hex.EncodeToString(other[:])
		}, want: ErrVerify},
		{name: "oversized body with content length", edit: func(m map[string]any) {
			m["platforms"].(map[string]any)[testPlatform].(map[string]any)["size"] = len(body) - 10
		}, want: ErrVerify},
		{name: "oversized body streamed", edit: func(m map[string]any) {
			m["platforms"].(map[string]any)[testPlatform].(map[string]any)["size"] = len(body) - 10
		}, hook: func(w http.ResponseWriter, _ *http.Request, b []byte) {
			// Flushing before the end makes the response chunked: no Content-Length to reject early.
			_, _ = w.Write(b[:100])
			w.(http.Flusher).Flush()
			_, _ = w.Write(b[100:])
		}, want: ErrVerify},
		{name: "truncated body", hook: func(w http.ResponseWriter, _ *http.Request, b []byte) {
			_, _ = w.Write(b[:100])
			w.(http.Flusher).Flush()
		}, want: ErrVerify},
		{name: "version mismatch", edit: func(m map[string]any) { m["version"] = "2.1.4" }, want: ErrVerify},
		{name: "no build for platform", edit: func(m map[string]any) {
			delete(m["platforms"].(map[string]any), testPlatform)
		}, want: ErrNotFound},
		{name: "unexpected binary name", edit: func(m map[string]any) {
			m["platforms"].(map[string]any)[testPlatform].(map[string]any)["binary"] = "../evil"
		}, want: ErrVerify},
		{name: "checksum not sha256", edit: func(m map[string]any) {
			m["platforms"].(map[string]any)[testPlatform].(map[string]any)["checksum"] = "abc"
		}, want: ErrVerify},
		{name: "size zero", edit: func(m map[string]any) {
			m["platforms"].(map[string]any)[testPlatform].(map[string]any)["size"] = 0
		}, want: ErrVerify},
		{name: "manifest not json", edit: func(m map[string]any) { m["platforms"] = "x" }, want: ErrVerify},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRelease(t)
			fr.add("2.1.5", body, tc.edit)
			fr.setBinaryHook(tc.hook)
			m := newManager(t, fr, t.TempDir(), nil)
			_, err := m.Ensure(context.Background(), "2.1.5")
			if !errors.Is(err, tc.want) {
				t.Fatalf("Ensure = %v, want %v", err, tc.want)
			}
			if _, err := os.Stat(filepath.Join(m.Dir(), "2.1.5")); !os.IsNotExist(err) {
				t.Fatalf("a failed install left a version directory: %v", err)
			}
			if len(m.Installed()) != 0 {
				t.Fatal("a failed install is listed")
			}
			assertNothingExecutable(t, m)
			assertNoLeftovers(t, m.Dir())
		})
	}
}

func TestPinNotFound(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	if _, err := m.Resolve(context.Background(), "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(9.9.9) = %v, want ErrNotFound", err)
	}
	// Manifest present but the binary 404s.
	fr.add("9.9.8", binary("9.9.8"), nil)
	fr.mu.Lock()
	delete(fr.binaries, "9.9.8")
	fr.mu.Unlock()
	if _, err := m.Resolve(context.Background(), "9.9.8"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(9.9.8) = %v, want ErrNotFound", err)
	}
}

func TestResolveRejectsInvalidRequests(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	for _, s := range []string{"rc", "v2.1.3", "2.1", "02.1.3", "2.1.3-beta", "../2.1.3", "2.1.3/x"} {
		if _, err := m.Resolve(context.Background(), s); !errors.Is(err, ErrInvalidVersion) {
			t.Errorf("Resolve(%q) = %v, want ErrInvalidVersion", s, err)
		}
	}
	if _, err := m.Ensure(context.Background(), "../x"); !errors.Is(err, ErrInvalidVersion) {
		t.Errorf("Ensure: %v", err)
	}
	if _, err := m.Channel(context.Background(), "rc"); !errors.Is(err, ErrInvalidVersion) {
		t.Errorf("Channel(rc): %v", err)
	}
	// A pointer that is not a version is refused, never used to build a path.
	fr.setPointer("latest", "../../etc/passwd")
	if _, err := m.Resolve(context.Background(), "latest"); !errors.Is(err, ErrNoVersion) {
		t.Errorf("garbage pointer: %v", err)
	}
	fr.setPointer("latest", strings.Repeat("1", 100))
	if _, err := m.Channel(context.Background(), "latest"); err == nil {
		t.Error("oversized pointer accepted")
	}
	if fr.manifestHits.Load() != 0 {
		t.Error("a manifest was fetched for an invalid version")
	}
}

func TestResolveChannel(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.1", binary("2.1.1"), nil)
	fr.add("2.1.2", binary("2.1.2"), nil)
	fr.setPointer("latest", "2.1.1")
	clock := &fakeClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	m := newManager(t, fr, dir, func(o *Options) { o.Now = clock.Now; o.Refresh = 15 * time.Minute })

	// Nothing cached: the launch waits for the install.
	r, err := m.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Requested != "latest" || r.Target != "2.1.1" || r.Version != "2.1.1" || r.Pending() {
		t.Fatalf("resolution %+v", r)
	}
	// Memoised: the pointer is not fetched again within refresh.
	fr.setPointer("latest", "2.1.2")
	if r, _ = m.Resolve(context.Background(), "latest"); r.Version != "2.1.1" || fr.pointerHits.Load() != 1 {
		t.Fatalf("memo not used: %+v, %d pointer hits", r, fr.pointerHits.Load())
	}
	// After refresh: the new target isn't cached, so the newest cached version runs and the target downloads.
	clock.Advance(16 * time.Minute)
	r, err = m.Resolve(context.Background(), "latest")
	if err != nil {
		t.Fatal(err)
	}
	if r.Target != "2.1.2" || r.Version != "2.1.1" || !r.Pending() {
		t.Fatalf("resolution %+v", r)
	}
	waitFor(t, "background install", func() bool { _, ok := m.lookup("2.1.2"); return ok })
	if r, _ = m.Resolve(context.Background(), "latest"); r.Version != "2.1.2" || r.Pending() {
		t.Fatalf("resolution %+v", r)
	}
	// The memo persists: a new manager on the same directory doesn't fetch while it is fresh.
	hits := fr.pointerHits.Load()
	m2 := newManager(t, fr, dir, func(o *Options) { o.Now = clock.Now; o.Refresh = 15 * time.Minute })
	if v, err := m2.Channel(context.Background(), "latest"); err != nil || v != "2.1.2" || fr.pointerHits.Load() != hits {
		t.Fatalf("persisted memo: %q %v (hits %d → %d)", v, err, hits, fr.pointerHits.Load())
	}
	if got := m2.Resolved(); got["latest"] != "2.1.2" {
		t.Fatalf("Resolved = %v", got)
	}
	// ResolvedTargets adds when the target was resolved (reported in capabilities, minor 11).
	if got := m2.ResolvedTargets()["latest"]; got.Version != "2.1.2" || !got.ResolvedAt.Equal(clock.Now()) {
		t.Fatalf("ResolvedTargets = %+v (now %s)", got, clock.Now())
	}
}

func TestPointerOfflineFallsBackToCached(t *testing.T) {
	fr := newFakeRelease(t)
	for _, v := range []string{"2.1.0", "2.1.1"} {
		fr.add(v, binary(v), nil)
	}
	fr.setPointer("stable", "2.1.0")
	fr.setPointer("latest", "2.1.1")
	clock := &fakeClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Now = clock.Now })
	for _, ch := range []string{"stable", "latest"} {
		if _, err := m.Resolve(context.Background(), ch); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, "background install of latest", func() bool { return len(m.Installed()) == 2 })
	clock.Advance(2 * time.Hour)
	fr.setPointerOK(false)

	// The last known target of the channel when it is cached…
	r, err := m.Resolve(context.Background(), "stable")
	if err != nil || r.Version != "2.1.0" || r.Target != "" || !r.Pending() {
		t.Fatalf("stable offline: %+v %v", r, err)
	}
	// …else the newest verified cached version.
	_ = os.Remove(filepath.Join(m.Dir(), memoFile))
	m.mu.Lock()
	m.memo = map[string]memoEntry{}
	m.mu.Unlock()
	if r, err = m.Resolve(context.Background(), "stable"); err != nil || r.Version != "2.1.1" {
		t.Fatalf("stable offline without memo: %+v %v", r, err)
	}

	// Nothing cached and offline: an error, never an unverified binary.
	empty := newManager(t, fr, t.TempDir(), nil)
	if _, err := empty.Resolve(context.Background(), "latest"); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("empty cache offline: %v", err)
	}
}

func TestConcurrentRequestsDownloadOnce(t *testing.T) {
	fr := newFakeRelease(t)
	body := binary("2.1.7")
	fr.add("2.1.7", body, nil)
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	fr.setBinaryHook(func(w http.ResponseWriter, _ *http.Request, b []byte) {
		started <- struct{}{}
		<-release
		_, _ = w.Write(b)
	})
	dir := t.TempDir()
	// Two managers on one directory stand in for a second agent: the flock keeps it out.
	m1 := newManager(t, fr, dir, nil)
	m2 := newManager(t, fr, dir, nil)

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		m := m1
		if i%2 == 1 {
			m = m2
		}
		wg.Go(func() {
			inst, err := m.Ensure(context.Background(), "2.1.7")
			if err == nil && inst.Version != "2.1.7" {
				err = fmt.Errorf("got %s", inst.Version)
			}
			errs <- err
		})
	}
	m1.Prefetch("2.1.7") // joins the in-flight install
	<-started
	time.Sleep(100 * time.Millisecond) // let the other callers pile up on the singleflight and the lock
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := fr.binaryHits.Load(); n != 1 {
		t.Fatalf("binary downloaded %d times, want 1", n)
	}
}

func TestCallerCancelDoesNotAbortSharedDownload(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.8", binary("2.1.8"), nil)
	release := make(chan struct{})
	fr.setBinaryHook(func(w http.ResponseWriter, _ *http.Request, b []byte) {
		<-release
		_, _ = w.Write(b)
	})
	m := newManager(t, fr, t.TempDir(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := m.Ensure(ctx, "2.1.8"); done <- err }()
	waitFor(t, "download start", func() bool { return fr.binaryHits.Load() == 1 })
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Ensure = %v", err)
	}
	close(release)
	if _, err := m.Ensure(context.Background(), "2.1.8"); err != nil {
		t.Fatal(err)
	}
	if n := fr.binaryHits.Load(); n != 1 {
		t.Fatalf("binary downloaded %d times, want 1", n)
	}
}

func TestCrashMidDownloadLeavesNothingExecutable(t *testing.T) {
	fr := newFakeRelease(t)
	body := binary("2.1.9")
	fr.add("2.1.9", body, nil)
	halfway := make(chan struct{})
	abort := make(chan struct{})
	fr.setBinaryHook(func(w http.ResponseWriter, _ *http.Request, b []byte) {
		w.Header().Set("Content-Length", fmt.Sprint(len(b)))
		_, _ = w.Write(b[:len(b)/2])
		w.(http.Flusher).Flush()
		close(halfway)
		<-abort
		panic(http.ErrAbortHandler) // the connection drops mid-body
	})
	m := newManager(t, fr, t.TempDir(), nil)
	done := make(chan error, 1)
	go func() { _, err := m.Ensure(context.Background(), "2.1.9"); done <- err }()
	<-halfway
	waitFor(t, "partial file on disk", func() bool {
		m, _ := filepath.Glob(filepath.Join(m.Dir(), "2.1.9.tmp-*", BinaryName))
		return len(m) == 1
	})
	// Mid-download: the partial file exists only in the temporary directory and is not executable.
	assertNothingExecutable(t, m)
	if _, err := os.Stat(filepath.Join(m.Dir(), "2.1.9")); !os.IsNotExist(err) {
		t.Fatal("version directory exists mid-download")
	}
	close(abort)
	if err := <-done; err == nil {
		t.Fatal("an interrupted download succeeded")
	}
	assertNoLeftovers(t, m.Dir())

	// A process killed mid-download leaves its temporary directory behind: it is never listed or resolved, and Clean
	// removes it.
	tmp := filepath.Join(m.Dir(), "2.1.9.tmp-123456")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, BinaryName), body[:10], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(m.Dir(), "2.1.1.gc-abc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if len(m.Installed()) != 0 {
		t.Fatal("a leftover is listed as installed")
	}
	assertNothingExecutable(t, m)
	// A temporary directory whose version another process is installing stays.
	busy := filepath.Join(m.Dir(), "2.1.8.tmp-999")
	_ = os.MkdirAll(busy, 0o700)
	unlock, ok, err := tryLockFile(m.lockPath("2.1.8"))
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := m.Clean(); err != nil {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(busy); err != nil {
		t.Fatal("Clean removed a temporary directory another process holds")
	}
	_ = os.RemoveAll(busy)
	assertNoLeftovers(t, m.Dir())
}

func TestVerifyRemovesDamagedVersion(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.3", binary("2.1.3"), nil)
	m := newManager(t, fr, t.TempDir(), nil)
	inst, err := m.Ensure(context.Background(), "2.1.3")
	if err != nil {
		t.Fatal(err)
	}
	// Same size, different content: only the hash catches it.
	b, _ := os.ReadFile(inst.Path)
	b[len(b)-1] ^= 1
	// Undo the read-only mode to damage it.
	_ = os.Chmod(inst.Path, 0o755)                            //nolint:gosec // test binary
	if err := os.WriteFile(inst.Path, b, 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	errs := m.Verify(context.Background(), "2.1.3")
	if !errors.Is(errs["2.1.3"], ErrVerify) {
		t.Fatalf("Verify = %v", errs)
	}
	if _, err := os.Stat(filepath.Join(m.Dir(), "2.1.3")); !os.IsNotExist(err) {
		t.Fatal("damaged version kept")
	}
	if _, err := m.Ensure(context.Background(), "2.1.3"); err != nil {
		t.Fatal(err)
	}
	if n := fr.binaryHits.Load(); n != 2 {
		t.Fatalf("binary downloaded %d times, want 2", n)
	}
	// A truncated binary fails even the cheap lookup.
	inst, _ = m.Ensure(context.Background(), "2.1.3")
	_ = os.Chmod(inst.Path, 0o755) //nolint:gosec // test binary
	if err := os.Truncate(inst.Path, 5); err != nil {
		t.Fatal(err)
	}
	if errs := m.Verify(context.Background(), "2.1.3"); errs["2.1.3"] == nil {
		t.Fatal("truncated binary verified")
	}
	// The kept manifest no longer matching verified.json is damage too (the binary itself is intact): one that lacks
	// the platform, and one whose content is equivalent but whose bytes (hash) differ.
	for _, tamper := range []func([]byte) []byte{
		func([]byte) []byte { return []byte(`{"version":"2.1.3"}`) },
		func(b []byte) []byte { return append(b, ' ') },
	} {
		inst, err = m.Ensure(context.Background(), "2.1.3")
		if err != nil {
			t.Fatal(err)
		}
		vdir := filepath.Dir(inst.Path)
		_ = os.Chmod(vdir, 0o700) //nolint:gosec // test: undo the read-only mode to tamper with it
		kept, _ := os.ReadFile(filepath.Join(vdir, manifestFile))
		_ = os.Remove(filepath.Join(vdir, manifestFile))
		if err := os.WriteFile(filepath.Join(vdir, manifestFile), tamper(kept), 0o400); err != nil {
			t.Fatal(err)
		}
		if errs := m.Verify(context.Background(), "2.1.3"); !errors.Is(errs["2.1.3"], ErrVerify) {
			t.Fatalf("tampered manifest.json: %v", errs)
		}
		if _, err := os.Lstat(vdir); !os.IsNotExist(err) {
			t.Fatal("version with a tampered manifest.json kept")
		}
	}
}

func TestGCKeepsNewestPinnedInUseAndChannelTargets(t *testing.T) {
	fr := newFakeRelease(t)
	versions := []string{"2.1.1", "2.1.2", "2.1.3", "2.1.10", "2.1.11", "2.1.12", "2.1.13"}
	for _, v := range versions {
		fr.add(v, binary(v), nil)
	}
	fr.setPointer("stable", "2.1.3")
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Keep = 3 })
	for _, v := range versions {
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Channel(context.Background(), "stable"); err != nil {
		t.Fatal(err)
	}
	// 2.1.10 is being installed or removed by another process: skipped.
	unlock, ok, err := tryLockFile(m.lockPath("2.1.10"))
	if err != nil || !ok {
		t.Fatal(err)
	}
	removed, err := m.GC(context.Background(), []string{"2.1.1"}, inUse("2.1.2"))
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed %v, want nothing", removed)
	}
	removed, err = m.GC(context.Background(), []string{"2.1.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(removed, ",") != "2.1.10,2.1.2" {
		t.Fatalf("removed %v", removed)
	}
	var left []string
	for _, inst := range m.Installed() {
		left = append(left, inst.Version)
	}
	if strings.Join(left, ",") != "2.1.13,2.1.12,2.1.11,2.1.3,2.1.1" {
		t.Fatalf("left %v", left)
	}
	assertNoLeftovers(t, m.Dir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.GC(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("GC with a cancelled context: %v", err)
	}
}

func TestRunPrefetchesChannels(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.20", binary("2.1.20"), nil)
	fr.add("2.1.21", binary("2.1.21"), nil)
	fr.setPointer("latest", "2.1.20")
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Refresh = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, "latest", "stable", "bogus"); close(done) }()
	waitFor(t, "prefetch of 2.1.20", func() bool { _, ok := m.lookup("2.1.20"); return ok })
	fr.setPointer("latest", "2.1.21")
	waitFor(t, "prefetch of 2.1.21", func() bool { _, ok := m.lookup("2.1.21"); return ok })
	cancel()
	<-done
}

func TestBaseURLMustBeHTTPS(t *testing.T) {
	dir := t.TempDir()
	for _, raw := range []string{
		"http://downloads.example.com/claude-code-releases", "ftp://x", "https://", "https://u:p@example.com/x",
		"https://example.com/x?q=1", "https://example.com/x#f", "://bad",
	} {
		if _, err := New(Options{Dir: dir, BaseURL: raw, Platform: testPlatform}); err == nil {
			t.Errorf("New accepted base URL %q", raw)
		}
	}
	if _, err := New(Options{Dir: "relative", Platform: testPlatform}); err == nil {
		t.Error("relative dir accepted")
	}
	if _, err := New(Options{Dir: dir, Platform: "win32-x64"}); err == nil {
		t.Error("unsupported platform accepted")
	}
	u, err := ParseBaseURL("https://mirror.example.com/claude/")
	if err != nil || u.String() != "https://mirror.example.com/claude" {
		t.Fatalf("ParseBaseURL: %v %v", u, err)
	}
	m, err := New(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if m.base.String() != DefaultBaseURL || m.keep != DefaultKeep || m.refresh != DefaultRefresh || m.timeout != DefaultDownloadTimeout {
		t.Fatalf("defaults: %v %d %v %v", m.base, m.keep, m.refresh, m.timeout)
	}
}

func TestRedirectToHTTPRefused(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "2.1.1")
	}))
	defer plain.Close()
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL+"/latest", http.StatusFound)
	}))
	defer redirecting.Close()
	m, err := New(Options{Dir: t.TempDir(), BaseURL: redirecting.URL, Platform: testPlatform, Client: redirecting.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.Channel(context.Background(), "latest"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("redirect to http: %v", err)
	}
}

func TestCloseStopsNewInstalls(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.30", binary("2.1.30"), nil)
	m := newManager(t, fr, t.TempDir(), nil)
	m.Close()
	if _, err := m.Ensure(context.Background(), "2.1.30"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Ensure after Close = %v", err)
	}
}

func TestVersionsAndPlatform(t *testing.T) {
	for _, v := range []string{"0.0.0", "2.1.322", "10.20.30"} {
		if !ValidVersion(v) || !ValidTarget(v) {
			t.Errorf("%q should be valid", v)
		}
	}
	for _, v := range []string{"", "2.1", "2.1.3.4", "v2.1.3", "01.1.1", "2.1.3-rc1", "2.1.3 ", "1234567.0.0", "latest"} {
		if ValidVersion(v) {
			t.Errorf("%q should be invalid", v)
		}
	}
	if !ValidTarget("latest") || !ValidTarget("stable") || ValidTarget("rc") {
		t.Error("ValidTarget channels")
	}
	if CompareVersions("2.1.10", "2.1.9") != 1 || CompareVersions("2.1.9", "2.10.0") != -1 || CompareVersions("1.2.3", "1.2.3") != 0 {
		t.Error("CompareVersions")
	}

	glibc, musl, alpine := t.TempDir(), t.TempDir(), t.TempDir()
	writeELF(t, filepath.Join(glibc, "bin", "sh"), "/lib64/ld-linux-x86-64.so.2")
	writeELF(t, filepath.Join(musl, "bin", "sh"), "/lib/ld-musl-x86_64.so.1")
	// Alpine: /bin/sh is a link to busybox, linked against musl.
	writeELF(t, filepath.Join(alpine, "bin", "busybox"), "/lib/ld-musl-aarch64.so.1")
	if err := os.Symlink("busybox", filepath.Join(alpine, "bin", "sh")); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ goos, goarch, root, want string }{
		{"linux", "amd64", glibc, "linux-x64"},
		{"linux", "arm64", glibc, "linux-arm64"},
		{"linux", "amd64", musl, "linux-x64-musl"},
		{"linux", "arm64", alpine, "linux-arm64-musl"},
		{"darwin", "arm64", glibc, "darwin-arm64"},
		{"darwin", "amd64", glibc, "darwin-x64"},
		{"windows", "amd64", glibc, ""},
		{"linux", "386", glibc, ""},
	}
	for _, c := range cases {
		got, err := platformFor(c.goos, c.goarch, c.root)
		if got != c.want || (c.want == "") != (err != nil) {
			t.Errorf("platformFor(%s,%s) = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
	// A Debian host with the musl package: musl's loader and even an alpine-release file exist, but /bin/sh is glibc's.
	debian := t.TempDir()
	writeELF(t, filepath.Join(debian, "bin", "sh"), "/lib/ld-linux-aarch64.so.1")
	for _, f := range []string{"lib/ld-musl-x86_64.so.1", "usr/lib/x86_64-linux-musl/libc.so", "etc/alpine-release"} {
		_ = os.MkdirAll(filepath.Join(debian, filepath.Dir(f)), 0o700)
		_ = os.WriteFile(filepath.Join(debian, f), nil, 0o600)
	}
	// Not a dynamic ELF: a script, a static binary (no PT_INTERP), nothing at all.
	script, static, none := t.TempDir(), t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(script, "bin"), 0o700)
	_ = os.WriteFile(filepath.Join(script, "bin", "sh"), []byte("#!/lib/ld-musl-x86_64.so.1\n"), 0o600)
	writeELF(t, filepath.Join(static, "bin", "sh"), "")
	for root, want := range map[string]bool{
		glibc: false, musl: true, alpine: true, debian: false, script: false, static: false, none: false,
	} {
		if got := isMusl(root); got != want {
			t.Errorf("isMusl(%s) = %v, want %v", root, got, want)
		}
	}
	if p, err := Platform(); err == nil && !platforms[p] {
		t.Errorf("Platform() = %q", p)
	}
}

// The node's cache and the protocol accept exactly the same exact versions (a profile pin is checked by central with
// proto.CheckHarnessVersion and used by the node with ValidVersion).
func TestValidVersionMatchesProto(t *testing.T) {
	for _, v := range []string{"", "0.0.0", "2.1.322", "02.1.3", "2.1", "2.1.3-rc1", "999999.1.1", "1000000.1.1", "latest"} {
		if ValidVersion(v) != proto.IsHarnessVersion(v) {
			t.Errorf("%q: dist %v, proto %v", v, ValidVersion(v), proto.IsHarnessVersion(v))
		}
	}
}
