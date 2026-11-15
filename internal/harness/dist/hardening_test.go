package dist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	binenc "encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeELF writes a minimal ELF64 executable at path whose PT_INTERP is interp (no PT_INTERP when interp is empty):
// enough for debug/elf, generated at test time instead of a binary fixture.
func writeELF(t *testing.T, path, interp string) {
	t.Helper()
	var buf bytes.Buffer
	hdr := elf.Header64{
		Type: uint16(elf.ET_DYN), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Ehsize: 64, Phentsize: 56, Shentsize: 64,
	}
	copy(hdr.Ident[:], elf.ELFMAG)
	hdr.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	hdr.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	hdr.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	data := append([]byte(interp), 0)
	if interp != "" {
		hdr.Phoff, hdr.Phnum = 64, 1
	}
	_ = binenc.Write(&buf, binenc.LittleEndian, hdr)
	if interp != "" {
		_ = binenc.Write(&buf, binenc.LittleEndian, elf.Prog64{
			Type: uint32(elf.PT_INTERP), Flags: uint32(elf.PF_R), Off: 64 + 56,
			Filesz: uint64(len(data)), Memsz: uint64(len(data)), Align: 1,
		})
		buf.Write(data)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o700); err != nil { //nolint:gosec // test fixture
		t.Fatal(err)
	}
}

func TestELFFixtureParses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sh")
	writeELF(t, p, "/lib/ld-musl-x86_64.so.1")
	f, err := elf.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if len(f.Progs) != 1 || f.Progs[0].Type != elf.PT_INTERP {
		t.Fatalf("progs %v", f.Progs)
	}
}

// chmodW makes a verified version directory and its files writable again so a test can damage them.
func chmodW(t *testing.T, m *Manager, version string) string {
	t.Helper()
	dir := filepath.Join(m.Dir(), version)
	_ = os.Chmod(dir, 0o700)                              //nolint:gosec // test
	_ = os.Chmod(filepath.Join(dir, BinaryName), 0o755)   //nolint:gosec // test
	_ = os.Chmod(filepath.Join(dir, verifiedFile), 0o600) //nolint:gosec // test
	_ = os.Chmod(filepath.Join(dir, manifestFile), 0o600) //nolint:gosec // test
	return dir
}

// S1: a <v> that fails lookup is removed under the lock and installed again in one download, and stays installed.
func TestEnsureReinstallsUnusableVersion(t *testing.T) {
	cases := map[string]func(t *testing.T, dir string){
		"no verified.json": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, verifiedFile)); err != nil {
				t.Fatal(err)
			}
		},
		"lost exec bit": func(t *testing.T, dir string) {
			if err := os.Chmod(filepath.Join(dir, BinaryName), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"size changed": func(t *testing.T, dir string) {
			f, err := os.OpenFile(filepath.Join(dir, BinaryName), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = f.WriteString("x")
			_ = f.Close()
		},
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			fr := newFakeRelease(t)
			fr.add("2.1.40", binary("2.1.40"), nil)
			m := newManager(t, fr, t.TempDir(), nil)
			if _, err := m.Ensure(context.Background(), "2.1.40"); err != nil {
				t.Fatal(err)
			}
			damage(t, chmodW(t, m, "2.1.40"))
			if _, ok := m.lookup("2.1.40"); ok {
				t.Fatal("damaged version passes lookup")
			}
			inst, err := m.Ensure(context.Background(), "2.1.40")
			if err != nil {
				t.Fatalf("reinstall: %v", err)
			}
			if got, _ := os.ReadFile(inst.Path); string(got) != string(binary("2.1.40")) {
				t.Fatal("reinstalled binary differs")
			}
			for range 3 {
				if _, err := m.Ensure(context.Background(), "2.1.40"); err != nil {
					t.Fatal(err)
				}
			}
			if n := fr.binaryHits.Load(); n != 2 {
				t.Fatalf("binary downloaded %d times, want 2 (install + one reinstall)", n)
			}
			assertNoLeftovers(t, m.Dir())
		})
	}

	// A channel target in that state downloads once in the background, then runs.
	fr := newFakeRelease(t)
	fr.add("2.1.41", binary("2.1.41"), nil)
	fr.add("2.1.42", binary("2.1.42"), nil)
	fr.setPointer("latest", "2.1.42")
	m := newManager(t, fr, t.TempDir(), nil)
	for _, v := range []string{"2.1.41", "2.1.42"} {
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(filepath.Join(chmodW(t, m, "2.1.42"), verifiedFile))
	r, err := m.Resolve(context.Background(), "latest")
	if err != nil || r.Version != "2.1.41" || r.Target != "2.1.42" {
		t.Fatalf("damaged target: %+v %v", r, err)
	}
	waitFor(t, "reinstall of the target", func() bool { _, ok := m.lookup("2.1.42"); return ok })
	for range 3 {
		if r, err = m.Resolve(context.Background(), "latest"); err != nil || r.Version != "2.1.42" || r.Pending() {
			t.Fatalf("after reinstall: %+v %v", r, err)
		}
	}
	m.Close() // waits for any background install
	if n := fr.binaryHits.Load(); n != 3 {
		t.Fatalf("binary downloaded %d times, want 3", n)
	}
}

// plantVersion builds a version directory by hand at dir: a binary with body and a verified.json that matches it.
func plantVersion(t *testing.T, dir, version string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, BinaryName), body, 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	writeVerified(t, dir, version, int64(len(body)))
}

func writeVerified(t *testing.T, dir, version string, size int64) {
	t.Helper()
	sum := sha256.Sum256([]byte("whatever"))
	b, _ := json.Marshal(Installed{
		Version: version, Platform: testPlatform, SHA256: hex.EncodeToString(sum[:]), Size: size,
		ManifestSHA256: hex.EncodeToString(sum[:]), FetchedAt: time.Now(),
	})
	_ = os.Remove(filepath.Join(dir, verifiedFile))
	if err := os.WriteFile(filepath.Join(dir, verifiedFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// S2: lookup opens <v> as a handle and refuses links; Installed and Ensure agree with it.
func TestLookupRefusesLinks(t *testing.T) {
	fr := newFakeRelease(t)
	relBin := binary("2.1.43")
	fr.add("2.1.43", relBin, nil)
	m := newManager(t, fr, t.TempDir(), nil)
	planted := []byte("#!/bin/sh\necho planted\n")

	// Control: the same hand-made layout as a real directory passes the cheap lookup.
	plantVersion(t, filepath.Join(m.Dir(), "2.1.44"), "2.1.44", planted)
	if _, ok := m.lookup("2.1.44"); !ok {
		t.Fatal("control: a hand-made real directory should pass lookup")
	}
	_ = os.RemoveAll(filepath.Join(m.Dir(), "2.1.44"))

	// <v> is a link to a planted directory with a hand-written verified.json.
	outside := t.TempDir()
	plantVersion(t, outside, "2.1.43", planted)
	if err := os.Symlink(outside, filepath.Join(m.Dir(), "2.1.43")); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.lookup("2.1.43"); ok {
		t.Fatal("lookup followed a linked version directory")
	}
	if len(m.Installed()) != 0 {
		t.Fatal("Installed lists a linked version directory")
	}
	inst, err := m.Ensure(context.Background(), "2.1.43")
	if err != nil {
		t.Fatal(err)
	}
	if fr.binaryHits.Load() != 1 {
		t.Fatal("Ensure returned the planted version without downloading")
	}
	if got, _ := os.ReadFile(inst.Path); string(got) != string(relBin) {
		t.Fatal("Ensure's binary is not the release binary")
	}
	if fi, err := os.Lstat(filepath.Join(m.Dir(), "2.1.43")); err != nil || !fi.IsDir() {
		t.Fatalf("the link was not replaced by a real directory: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(outside, BinaryName)); string(got) != string(planted) {
		t.Fatal("removing the link touched its target")
	}

	// Links that stay inside the cache, which os.Root alone would follow: <v> → another version directory holding a
	// hand-written verified.json for <v> (only the IsDir/SameFile checks refuse it)…
	inside := filepath.Join(m.Dir(), "2.1.49")
	plantVersion(t, inside, "2.1.46", planted)
	if err := os.Symlink("2.1.49", filepath.Join(m.Dir(), "2.1.46")); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.lookup("2.1.46"); ok {
		t.Fatal("lookup followed a version directory linked inside the cache")
	}
	_ = os.Remove(filepath.Join(m.Dir(), "2.1.46"))
	// …and verified.json → another file in the same directory (only the regular-file/SameFile checks refuse it).
	vdir := chmodW(t, m, "2.1.43")
	_ = os.Rename(filepath.Join(vdir, verifiedFile), filepath.Join(vdir, "elsewhere.json"))
	if err := os.Symlink("elsewhere.json", filepath.Join(vdir, verifiedFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.lookup("2.1.43"); ok {
		t.Fatal("lookup followed a linked verified.json")
	}
	_ = os.Remove(filepath.Join(vdir, verifiedFile))
	if err := os.Symlink(filepath.Join(outside, verifiedFile), filepath.Join(vdir, verifiedFile)); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.lookup("2.1.43"); ok {
		t.Fatal("lookup followed a verified.json linked outside the cache")
	}

	// Verify treats a linked <v> as damage and removes the link only.
	_ = os.RemoveAll(vdir)
	if err := os.Symlink(outside, filepath.Join(m.Dir(), "2.1.45")); err != nil {
		t.Fatal(err)
	}
	if errs := m.Verify(context.Background(), "2.1.45"); !errors.Is(errs["2.1.45"], ErrVerify) {
		t.Fatalf("Verify of a linked version: %v", errs)
	}
	if _, err := os.Lstat(filepath.Join(m.Dir(), "2.1.45")); !os.IsNotExist(err) {
		t.Fatal("linked version kept")
	}
	if _, err := os.Stat(filepath.Join(outside, BinaryName)); err != nil {
		t.Fatal("Verify removed the link's target")
	}
}

// lookup's own binary checks, through lookup (not Verify).
func TestLookupBinaryChecks(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.46", binary("2.1.46"), nil)
	m := newManager(t, fr, t.TempDir(), nil)
	inst, err := m.Ensure(context.Background(), "2.1.46")
	if err != nil {
		t.Fatal(err)
	}
	vdir := chmodW(t, m, "2.1.46")
	if _, ok := m.lookup("2.1.46"); !ok {
		t.Fatal("control: writable modes alone don't fail lookup")
	}

	// Size: one byte more, still an executable regular file.
	f, _ := os.OpenFile(inst.Path, os.O_WRONLY|os.O_APPEND, 0)
	_, _ = f.WriteString("x")
	_ = f.Close()
	if _, ok := m.lookup("2.1.46"); ok {
		t.Fatal("lookup accepted a binary of another size")
	}

	// Regular file: the binary is a link whose own size is what verified.json records; links are mode 0777, so only
	// the regular-file check refuses it.
	target := filepath.Join(t.TempDir(), "claude-elsewhere")
	if err := os.WriteFile(target, binary("2.1.46"), 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	_ = os.Remove(inst.Path)
	if err := os.Symlink(target, inst.Path); err != nil {
		t.Fatal(err)
	}
	writeVerified(t, vdir, "2.1.46", int64(len(target)))
	if _, ok := m.lookup("2.1.46"); ok {
		t.Fatal("lookup accepted a linked binary")
	}
	if len(m.Installed()) != 0 {
		t.Fatal("Installed disagrees with lookup")
	}
}

// countingTransport counts the binary body bytes the client actually reads from the server.
type countingTransport struct {
	rt http.RoundTripper
	n  *atomic.Int64
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

func (c countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.rt.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/"+BinaryName) {
		resp.Body = countingBody{resp.Body, c.n}
	}
	return resp, err
}

// Each size guard on its own: the Content-Length check (nothing read), the size+1 read cap (the stream stops), and
// the n > size check (a body whose first size+1 bytes match the manifest's checksum).
func TestDownloadSizeGuards(t *testing.T) {
	body := binary("2.1.47")
	size := len(body) - 10
	extra := bytes.Repeat([]byte("y"), 1<<20)
	chunked := func(w http.ResponseWriter, _ *http.Request, b []byte) {
		_, _ = w.Write(b[:100])
		w.(http.Flusher).Flush() // no Content-Length
		_, _ = w.Write(b[100:])
		_, _ = w.Write(extra) // far more than size+1
	}
	sumPlus1 := sha256.Sum256(body[:size+1])
	cases := []struct {
		name     string
		checksum string // "" keeps the full body's
		hook     func(http.ResponseWriter, *http.Request, []byte)
		wantMsg  string
		wantRead int64
	}{
		{name: "content length", hook: func(w http.ResponseWriter, _ *http.Request, b []byte) {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			_, _ = w.Write(b)
		}, wantMsg: "announces", wantRead: 0},
		{name: "stream stops at size+1", hook: chunked, wantMsg: "larger than", wantRead: int64(size) + 1},
		{name: "first size+1 bytes match", checksum: hex.EncodeToString(sumPlus1[:]), hook: chunked,
			wantMsg: "larger than", wantRead: int64(size) + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRelease(t)
			fr.add("2.1.47", body, func(m map[string]any) {
				p := m["platforms"].(map[string]any)[testPlatform].(map[string]any)
				p["size"] = size
				if tc.checksum != "" {
					p["checksum"] = tc.checksum
				}
			})
			fr.setBinaryHook(tc.hook)
			var read atomic.Int64
			m := newManager(t, fr, t.TempDir(), func(o *Options) {
				o.Client = &http.Client{Transport: countingTransport{rt: fr.srv.Client().Transport, n: &read}}
			})
			_, err := m.Ensure(context.Background(), "2.1.47")
			if !errors.Is(err, ErrVerify) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Ensure = %v, want ErrVerify %q", err, tc.wantMsg)
			}
			if got := read.Load(); got != tc.wantRead {
				t.Fatalf("read %d body bytes, want %d", got, tc.wantRead)
			}
			assertNothingExecutable(t, m)
			assertNoLeftovers(t, m.Dir())
		})
	}
}

// Manifest checks that reject before any download: the size range and the checksum format.
func TestManifestRejectedBeforeDownload(t *testing.T) {
	body := binary("2.1.48")
	sum := sha256.Sum256(body)
	realSum := hex.EncodeToString(sum[:])
	cases := []struct {
		name      string
		edit      func(p map[string]any)
		wantMsg   string
		downloads int32
	}{
		{name: "size above the cap", edit: func(p map[string]any) { p["size"] = int64(maxBinarySize) + 1 },
			wantMsg: "out of range"},
		// At the cap the manifest passes and the download runs (and then fails: the body is short).
		{name: "size at the cap", edit: func(p map[string]any) { p["size"] = int64(maxBinarySize) },
			wantMsg: "the manifest says", downloads: 1},
		// The binary's real checksum, upper case: only the format check catches it before downloading.
		{name: "checksum upper case", edit: func(p map[string]any) { p["checksum"] = strings.ToUpper(realSum) },
			wantMsg: "not a SHA-256"},
		{name: "checksum with a suffix", edit: func(p map[string]any) { p["checksum"] = realSum + "0" },
			wantMsg: "not a SHA-256"},
		{name: "checksum with a prefix", edit: func(p map[string]any) { p["checksum"] = "0" + realSum },
			wantMsg: "not a SHA-256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fr := newFakeRelease(t)
			fr.add("2.1.48", body, func(m map[string]any) {
				tc.edit(m["platforms"].(map[string]any)[testPlatform].(map[string]any))
			})
			m := newManager(t, fr, t.TempDir(), nil)
			_, err := m.Ensure(context.Background(), "2.1.48")
			if !errors.Is(err, ErrVerify) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Ensure = %v, want ErrVerify %q", err, tc.wantMsg)
			}
			if n := fr.binaryHits.Load(); n != tc.downloads {
				t.Fatalf("binary requested %d times, want %d", n, tc.downloads)
			}
		})
	}
}

// The channel pointer is capped at 64 bytes, whitespace included.
func TestPointerSizeCap(t *testing.T) {
	fr := newFakeRelease(t)
	m := newManager(t, fr, t.TempDir(), nil)
	// The fake appends "\n": 63 characters make a 64-byte body.
	fr.setPointer("latest", "2.1.1"+strings.Repeat(" ", 58))
	if v, err := m.refreshChannel(context.Background(), "latest"); err != nil || v != "2.1.1" {
		t.Fatalf("64-byte pointer: %q %v", v, err)
	}
	fr.setPointer("latest", "2.1.1"+strings.Repeat(" ", 59))
	if _, err := m.refreshChannel(context.Background(), "latest"); err == nil || !strings.Contains(err.Error(), "larger than 64") {
		t.Fatalf("65-byte pointer: %v", err)
	}
}

// loadMemo keeps only known channels with valid versions, and drops an invalid previous target.
func TestLoadMemoValidates(t *testing.T) {
	fr := newFakeRelease(t)
	dir := t.TempDir()
	at := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	saved := map[string]memoEntry{
		"latest": {Version: "../../etc", ResolvedAt: at},
		"rc":     {Version: "2.1.1", ResolvedAt: at},
		"stable": {Version: "2.1.0", Previous: "2.1.0-x", ResolvedAt: at},
	}
	b, _ := json.Marshal(saved)
	if err := os.WriteFile(filepath.Join(dir, memoFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, fr, dir, nil)
	got := m.Resolved()
	if len(got) != 1 || got["stable"] != "2.1.0" {
		t.Fatalf("Resolved = %v, want only stable", got)
	}
	if p := m.previousTargets(); len(p) != 0 {
		t.Fatalf("invalid previous target kept: %v", p)
	}
}

// N1: a channel never falls back to a version newer than its target.
func TestChannelFallbackNeverNewer(t *testing.T) {
	fr := newFakeRelease(t)
	for _, v := range []string{"2.1.1", "2.1.5", "2.1.6", "2.1.9"} {
		fr.add(v, binary(v), nil)
	}
	gate1 := make(chan struct{})
	fr.setBinaryHook(func(w http.ResponseWriter, r *http.Request, b []byte) {
		if strings.Contains(r.URL.Path, "/2.1.5/") {
			<-gate1
		}
		_, _ = w.Write(b)
	})
	clock := &fakeClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	m := newManager(t, fr, dir, func(o *Options) { o.Now = clock.Now; o.Refresh = 15 * time.Minute })
	for _, v := range []string{"2.1.1", "2.1.9"} { // 2.1.9 is cached for a pin
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	fr.setPointer("stable", "2.1.5")
	r, err := m.Resolve(context.Background(), "stable")
	if err != nil || r.Target != "2.1.5" || r.Version != "2.1.1" || r.Fallback != FallbackOlderCached {
		t.Fatalf("target not cached: %+v %v", r, err)
	}
	close(gate1)
	waitFor(t, "install of 2.1.5", func() bool { _, ok := m.lookup("2.1.5"); return ok })
	if r, _ = m.Resolve(context.Background(), "stable"); r.Version != "2.1.5" || r.Fallback != "" {
		t.Fatalf("target cached: %+v", r)
	}

	// The target moves: its previous target runs while the new one downloads.
	gate2 := make(chan struct{})
	fr.setBinaryHook(func(w http.ResponseWriter, r *http.Request, b []byte) {
		if strings.Contains(r.URL.Path, "/2.1.6/") {
			<-gate2
		}
		_, _ = w.Write(b)
	})
	fr.setPointer("stable", "2.1.6")
	clock.Advance(16 * time.Minute)
	if r, _ = m.Resolve(context.Background(), "stable"); r.Version != "2.1.5" || r.Fallback != FallbackPreviousTarget {
		t.Fatalf("new target downloading: %+v", r)
	}
	// Pointer down: the last known target (2.1.6) is not cached, so its previous target runs.
	clock.Advance(16 * time.Minute)
	fr.setPointerOK(false)
	if r, err = m.Resolve(context.Background(), "stable"); err != nil || r.Version != "2.1.5" || r.Target != "" ||
		r.Fallback != FallbackPreviousTarget {
		t.Fatalf("offline: %+v %v", r, err)
	}
	close(gate2)
	m.Close()

	// Only a newer version cached: a channel launch waits for its target rather than run the newer one.
	fr.setPointerOK(true)
	fr.setBinaryHook(nil)
	m2 := newManager(t, fr, t.TempDir(), nil)
	if _, err := m2.Ensure(context.Background(), "2.1.9"); err != nil {
		t.Fatal(err)
	}
	if r, err = m2.Resolve(context.Background(), "stable"); err != nil || r.Version != "2.1.6" || r.Fallback != "" {
		t.Fatalf("only newer cached: %+v %v", r, err)
	}
	// Offline with a stale target and only newer versions cached: nothing runs.
	clock2 := &fakeClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	dir3 := t.TempDir()
	b, _ := json.Marshal(map[string]memoEntry{"stable": {Version: "2.1.5", ResolvedAt: clock2.t}})
	_ = os.WriteFile(filepath.Join(dir3, memoFile), b, 0o600)
	m3 := newManager(t, fr, dir3, func(o *Options) { o.Now = clock2.Now })
	if _, err := m3.Ensure(context.Background(), "2.1.9"); err != nil {
		t.Fatal(err)
	}
	clock2.Advance(2 * time.Hour)
	fr.setPointerOK(false)
	if _, err := m3.Resolve(context.Background(), "stable"); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("offline, only newer cached: %v", err)
	}
	// No target ever known: any cached version, reported as such.
	_ = os.Remove(filepath.Join(dir3, memoFile))
	m3.mu.Lock()
	m3.memo = map[string]memoEntry{}
	m3.mu.Unlock()
	if r, err = m3.Resolve(context.Background(), "stable"); err != nil || r.Version != "2.1.9" || r.Fallback != FallbackNewestCached {
		t.Fatalf("offline, nothing known: %+v %v", r, err)
	}
}

func TestGCKeepsPreviousTarget(t *testing.T) {
	fr := newFakeRelease(t)
	for _, v := range []string{"2.1.60", "2.1.61", "2.1.62", "2.1.63"} {
		fr.add(v, binary(v), nil)
	}
	clock := &fakeClock{t: time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)}
	m := newManager(t, fr, t.TempDir(), func(o *Options) { o.Now = clock.Now; o.Keep = 1 })
	for _, v := range []string{"2.1.60", "2.1.61", "2.1.62", "2.1.63"} {
		if _, err := m.Ensure(context.Background(), v); err != nil {
			t.Fatal(err)
		}
	}
	fr.setPointer("stable", "2.1.60")
	_, _ = m.Channel(context.Background(), "stable")
	clock.Advance(2 * time.Hour)
	fr.setPointer("stable", "2.1.61")
	_, _ = m.Channel(context.Background(), "stable")
	removed, err := m.GC(context.Background(), nil, nil)
	if err != nil || strings.Join(removed, ",") != "2.1.62" {
		t.Fatalf("GC removed %v (%v), want 2.1.62 only", removed, err)
	}
	assertNoLeftovers(t, m.Dir())
}

// N2: redirects stay on the base URL's host.
func TestRedirectsStayOnBaseHost(t *testing.T) {
	other := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "2.1.1")
	}))
	other.Config.ErrorLog = log.New(io.Discard, "", 0)
	other.StartTLS()
	defer other.Close()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rel/latest":
			http.Redirect(w, r, "/rel/moved", http.StatusFound)
		case "/rel/moved":
			_, _ = fmt.Fprintln(w, "2.1.2")
		case "/rel/stable":
			http.Redirect(w, r, other.URL+"/rel/stable", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()
	m, err := New(Options{Dir: t.TempDir(), BaseURL: srv.URL + "/rel", Platform: testPlatform, Client: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if v, err := m.Channel(context.Background(), "latest"); err != nil || v != "2.1.2" {
		t.Fatalf("same-host redirect: %q %v", v, err)
	}
	if _, err := m.Channel(context.Background(), "stable"); err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("redirect to another https host: %v", err)
	}
}

// N4: a link planted at a lock's name is refused, not followed.
func TestLockRefusesLink(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.70", binary("2.1.70"), nil)
	m := newManager(t, fr, t.TempDir(), nil)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Symlink(target, m.lockPath("2.1.70")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := tryLockFile(m.lockPath("2.1.70")); err == nil {
		t.Fatal("tryLockFile followed a link")
	}
	if _, err := m.Ensure(context.Background(), "2.1.70"); err == nil {
		t.Fatal("Ensure installed through a linked lock")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("the link's target was created")
	}
}

// N5: New refuses an unsafe existing cache directory.
func TestNewRefusesUnsafeCacheDir(t *testing.T) {
	base := t.TempDir()
	good := filepath.Join(base, "good")
	if err := os.Mkdir(good, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	_ = os.Symlink(good, link)
	file := filepath.Join(base, "file")
	_ = os.WriteFile(file, nil, 0o600)
	groupW := filepath.Join(base, "group")
	_ = os.Mkdir(groupW, 0o700)
	_ = os.Chmod(groupW, 0o770) //nolint:gosec // test: an unsafe mode
	worldW := filepath.Join(base, "world")
	_ = os.Mkdir(worldW, 0o700)
	_ = os.Chmod(worldW, 0o702) //nolint:gosec // test: an unsafe mode
	cases := map[string]string{link: "symbolic link", file: "", groupW: "writable", worldW: "writable"}
	if os.Geteuid() != 0 {
		cases["/"] = "owned by another user" // 0755, root's
	}
	for dir, want := range cases {
		_, err := New(Options{Dir: dir, Platform: testPlatform})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("New(%s) = %v, want %q", dir, err, want)
		}
	}
	readable := filepath.Join(base, "readable")
	_ = os.Mkdir(readable, 0o700)
	_ = os.Chmod(readable, 0o755) //nolint:gosec // test: readable by others is fine
	for _, dir := range []string{good, readable, filepath.Join(base, "new", "nested")} {
		m, err := New(Options{Dir: dir, Platform: testPlatform})
		if err != nil {
			t.Errorf("New(%s): %v", dir, err)
			continue
		}
		m.Close()
	}
	if fi, err := os.Stat(filepath.Join(base, "new", "nested")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("created cache dir: %v %v", fi, err)
	}
}

// GC and Clean delete read-only version directories.
func TestRemovalHandlesReadOnlyDirectories(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.80", binary("2.1.80"), nil)
	m := newManager(t, fr, t.TempDir(), nil)
	if _, err := m.Ensure(context.Background(), "2.1.80"); err != nil {
		t.Fatal(err)
	}
	// A crashed removal: a read-only directory renamed to .gc-*.
	gone := filepath.Join(m.Dir(), "2.1.80.gc-abc")
	if err := os.Rename(filepath.Join(m.Dir(), "2.1.80"), gone); err != nil {
		t.Fatal(err)
	}
	if err := m.Clean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(gone); !os.IsNotExist(err) {
		t.Fatal("Clean left a read-only .gc directory")
	}
	if _, err := m.Ensure(context.Background(), "2.1.80"); err != nil {
		t.Fatal(err)
	}
	if err := m.removeLocked(context.Background(), "2.1.80"); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(m.Dir())
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("left %s", e.Name())
		}
	}
}

// N3: a replaced binary with verified.json rewritten to match it is caught by the kept release manifest.
func TestVerifyCatchesForgedVerifiedJSON(t *testing.T) {
	fr := newFakeRelease(t)
	fr.add("2.1.90", binary("2.1.90"), nil)
	m := newManager(t, fr, t.TempDir(), nil)
	inst, err := m.Ensure(context.Background(), "2.1.90")
	if err != nil {
		t.Fatal(err)
	}
	vdir := chmodW(t, m, "2.1.90")
	forged := bytes.Repeat([]byte("z"), int(inst.Size))            // same size, other content
	if err := os.WriteFile(inst.Path, forged, 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	sum := sha256.Sum256(forged)
	inst.SHA256 = hex.EncodeToString(sum[:]) // ManifestSHA256 stays the real manifest's
	b, _ := json.Marshal(inst)
	if err := os.WriteFile(filepath.Join(vdir, verifiedFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.lookup("2.1.90"); !ok {
		t.Fatal("control: the forgery passes the cheap lookup")
	}
	if errs := m.Verify(context.Background(), "2.1.90"); !errors.Is(errs["2.1.90"], ErrVerify) {
		t.Fatalf("Verify accepted a forged verified.json: %v", errs)
	}
}
