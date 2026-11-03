package artifacts_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/config"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

const (
	bucket = "sm-artifacts"
	run1   = "run_0123456789abcdefghijklmnop"
	run2   = "run_zyxwvutsrqponmlkjihgfedcba"
)

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func localStore(t *testing.T) (*artifacts.Local, string) {
	t.Helper()
	dir := t.TempDir()
	return artifacts.NewLocal(map[string]string{artifacts.NSAttachments: filepath.Join(dir, "artifacts"),
		artifacts.NSRecordings: filepath.Join(dir, "recordings"), artifacts.NSTranscripts: filepath.Join(dir, "transcripts")}), dir
}

func s3Store(t *testing.T) (*artifacts.S3, *s3test.Server) {
	t.Helper()
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: bucket, PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	return artifacts.NewS3(cl, bucket, "sm/art/"), srv
}

func put(t *testing.T, s artifacts.Store, key string, data []byte) {
	t.Helper()
	if err := s.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), sum(data)); err != nil {
		t.Fatalf("put %s: %v", key, err)
	}
}

func readAll(t *testing.T, s artifacts.Store, key string) []byte {
	t.Helper()
	o, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatalf("open %s: %v", key, err)
	}
	defer o.Close()
	b, err := io.ReadAll(o)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return b
}

// contract is what every Store does (SM-ARCH-003): the same behavior from the local disk and from S3.
func contract(t *testing.T, s artifacts.Store) {
	ctx := context.Background()
	data := random(t, 300<<10)
	att := artifacts.AttachmentKey(sum(data))
	seg := artifacts.RecordingKey(run1, "seg-000001.smrec.zst")
	tr := artifacts.TranscriptKey(run1)
	put(t, s, att, data)
	put(t, s, seg, []byte("segment one"))
	put(t, s, artifacts.RecordingKey(run1, "seg-000002.smrec.zst"), []byte("segment two"))
	put(t, s, tr, []byte(`{"line":1}`+"\n"))

	if got := readAll(t, s, att); !bytes.Equal(got, data) {
		t.Fatal("attachment round trip differs")
	}
	info, err := s.Stat(ctx, att)
	if err != nil || info.Size != int64(len(data)) || info.Key != att || info.ModTime.IsZero() {
		t.Fatalf("stat %+v %v", info, err)
	}
	// Seeking reads from an offset (range requests on S3), and seeking back starts over.
	o, err := s.Open(ctx, att)
	if err != nil {
		t.Fatal(err)
	}
	if o.Info().Size != int64(len(data)) {
		t.Fatalf("open info %+v", o.Info())
	}
	if n, err := o.Seek(-10, io.SeekEnd); err != nil || n != int64(len(data))-10 {
		t.Fatalf("seek end %d %v", n, err)
	}
	tail, _ := io.ReadAll(o)
	if !bytes.Equal(tail, data[len(data)-10:]) {
		t.Fatalf("tail %x", tail)
	}
	if _, err := o.Seek(1000, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	mid := make([]byte, 5)
	if _, err := io.ReadFull(o, mid); err != nil || !bytes.Equal(mid, data[1000:1005]) {
		t.Fatalf("mid %x %v", mid, err)
	}
	if n, err := o.Seek(2, io.SeekCurrent); err != nil || n != 1007 {
		t.Fatalf("seek current %d %v", n, err)
	}
	if _, err := o.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if all, _ := io.ReadAll(o); !bytes.Equal(all, data) {
		t.Fatal("re-read from the start differs")
	}
	_ = o.Close()

	// Listing is by prefix, in key order, artifacts only.
	ls, err := s.List(ctx, artifacts.RecordingPrefix(run1))
	if err != nil || len(ls) != 2 || ls[0].Key != seg || ls[1].Size != int64(len("segment two")) {
		t.Fatalf("list recordings %+v %v", ls, err)
	}
	all, err := s.List(ctx, "")
	if err != nil || len(all) != 4 || all[0].Key != att || all[3].Key != tr {
		t.Fatalf("list all %+v %v", all, err)
	}
	if ls, _ := s.List(ctx, artifacts.NSTranscripts+"/"); len(ls) != 1 {
		t.Fatalf("list transcripts %+v", ls)
	}

	// Content that does not match its SHA-256 is refused and nothing is stored.
	bad := artifacts.TranscriptKey(run2)
	if err := s.Put(ctx, bad, strings.NewReader("abc"), 3, sum([]byte("abd"))); err == nil {
		t.Fatal("mismatched content stored")
	}
	if _, err := s.Stat(ctx, bad); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("mismatched content left behind: %v", err)
	}
	if err := s.Put(ctx, "attachments/tmp/upload-1", strings.NewReader("x"), 1, sum([]byte("x"))); err == nil {
		t.Fatal("a non-artifact key was stored")
	}
	if _, err := s.Open(ctx, bad); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("open missing: %v", err)
	}

	// Deletes (retention and purge); deleting a missing key succeeds.
	for _, k := range []string{seg, artifacts.RecordingKey(run1, "seg-000002.smrec.zst"), tr, tr} {
		if err := s.Delete(ctx, k); err != nil {
			t.Fatalf("delete %s: %v", k, err)
		}
	}
	if err := s.Delete(ctx, "../escape"); err == nil {
		t.Fatal("invalid key deleted")
	}
	if ls, _ := s.List(ctx, ""); len(ls) != 1 || ls[0].Key != att {
		t.Fatalf("after deletes %+v", ls)
	}
	if s.Kind() == "" || s.Location() == "" {
		t.Fatal("kind and location")
	}
}

func TestLocalStore(t *testing.T) {
	s, dir := localStore(t)
	contract(t, s)
	// The local layout is the one SpaceMan always used.
	data := []byte("hello")
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "sha256", sum(data)[:2], sum(data))); err == nil {
		t.Fatal("unexpected")
	}
	put(t, s, artifacts.AttachmentKey(sum(data)), data)
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "sha256", sum(data)[:2], sum(data))); err != nil {
		t.Fatalf("local layout: %v", err)
	}
	// A recording's run folder goes with its last segment.
	put(t, s, artifacts.RecordingKey(run2, "seg-000001.smrec.zst"), []byte("x"))
	if err := s.Delete(context.Background(), artifacts.RecordingKey(run2, "seg-000001.smrec.zst")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "recordings", run2)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("empty run folder left: %v", err)
	}
	// Temporary and partial files are not artifacts.
	if err := os.WriteFile(filepath.Join(dir, "transcripts", run2+".jsonl.part"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ls, _ := s.List(context.Background(), artifacts.NSTranscripts+"/"); len(ls) != 0 {
		t.Fatalf("partial file listed: %+v", ls)
	}
	if !strings.Contains(s.Location(), "recordings=") || s.Root(artifacts.NSRecordings) != filepath.Join(dir, "recordings") {
		t.Fatalf("location %q", s.Location())
	}
	if _, err := artifacts.NewLocal(nil).Path(artifacts.TranscriptKey(run1)); err == nil {
		t.Fatal("a namespace without a directory was accepted")
	}
	if err := s.Put(context.Background(), artifacts.TranscriptKey(run2), strings.NewReader("ab"), 3, ""); err == nil {
		t.Fatal("short content stored")
	}
}

func TestS3Store(t *testing.T) {
	s, srv := s3Store(t)
	contract(t, s)
	// Objects live under the prefix, keyed exactly like the local layout.
	keys := srv.Keys(bucket)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "sm/art/attachments/sha256/") {
		t.Fatalf("bucket keys %v", keys)
	}
	if srv.Ranges == 0 {
		t.Fatal("seeking did not use range requests")
	}
	// Foreign objects under the prefix are not artifacts.
	srv.SetObject(bucket, "sm/art/attachments/notes.txt", []byte("x"))
	if ls, _ := s.List(context.Background(), ""); len(ls) != 1 {
		t.Fatalf("foreign object listed: %+v", ls)
	}
	// An upload needs the content's hash.
	if err := s.Put(context.Background(), artifacts.TranscriptKey(run1), strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("upload without a hash")
	}
	if s.Location() != "s3://"+bucket+"/sm/art/" {
		t.Fatalf("location %q", s.Location())
	}
	// An object that shrinks under a reader ends with an error, never a short success.
	key := artifacts.TranscriptKey(run1)
	put(t, s, key, []byte("0123456789"))
	o, err := s.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetObject(bucket, "sm/art/"+key, []byte("01234"))
	if _, err := io.ReadAll(o); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("shrunk object: %v", err)
	}
	_ = o.Close()
	o, _ = s.Open(context.Background(), key)
	if _, err := o.Seek(0, 42); err == nil {
		t.Fatal("bad whence")
	}
	if _, err := o.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative seek")
	}
	_ = o.Close()
	// Storage errors are reported, not hidden.
	srv.Fail = func(*http.Request) int { return http.StatusInternalServerError }
	if _, err := s.Stat(context.Background(), key); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("storage error: %v", err)
	}
	if _, err := s.List(context.Background(), ""); err == nil {
		t.Fatal("list error hidden")
	}
}

// Verified fails a full sequential read of damaged content at its end and withholds the last bytes.
func TestVerified(t *testing.T) {
	for name, s := range map[string]artifacts.Store{"local": func() artifacts.Store { s, _ := localStore(t); return s }(),
		"s3": func() artifacts.Store { s, _ := s3Store(t); return s }()} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			data := random(t, 70<<10)
			key := artifacts.TranscriptKey(run1)
			put(t, s, key, data)
			o, _ := s.Open(ctx, key)
			v := artifacts.Verified(o, sum(data))
			if got, err := io.ReadAll(v); err != nil || !bytes.Equal(got, data) {
				t.Fatalf("intact read: %v", err)
			}
			_ = v.Close()
			o, _ = s.Open(ctx, key)
			v = artifacts.Verified(o, sum([]byte("other")))
			got, err := io.ReadAll(v)
			if !errors.Is(err, artifacts.ErrChecksum) || len(got) >= len(data) {
				t.Fatalf("damaged read: %d bytes, %v", len(got), err)
			}
			// Like http.ServeContent: size by seeking to the end, back to the start, a read bounded by the size.
			if _, err := v.Seek(0, io.SeekEnd); err != nil {
				t.Fatal(err)
			}
			if _, err := v.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(io.Discard, io.LimitReader(v, int64(len(data)))); !errors.Is(err, artifacts.ErrChecksum) {
				t.Fatalf("bounded damaged read: %v", err)
			}
			// A range read is not checked.
			if _, err := v.Seek(100, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if b, err := io.ReadAll(v); err != nil || !bytes.Equal(b, data[100:]) {
				t.Fatalf("range read: %v", err)
			}
			_ = v.Close()
		})
	}
}

func TestPutFileAndMigrate(t *testing.T) {
	ctx := context.Background()
	local, dir := localStore(t)
	remote, srv := s3Store(t)
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	spool := func(data []byte) string {
		f, err := os.CreateTemp(tmp, "spool-*")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
		_ = f.Close()
		return f.Name()
	}
	// The local store renames a spool file into place; the S3 store uploads it and removes it.
	data := random(t, 4096)
	p := spool(data)
	if err := artifacts.PutFile(ctx, local, artifacts.AttachmentKey(sum(data)), p, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("spool file left after a local put")
	}
	lp, _ := local.Path(artifacts.AttachmentKey(sum(data)))
	if err := artifacts.PutFile(ctx, local, artifacts.AttachmentKey(sum(data)), lp, int64(len(data)), sum(data)); err != nil {
		t.Fatalf("moving the object onto itself: %v", err)
	}
	p = spool([]byte("transcript"))
	if err := artifacts.PutFile(ctx, remote, artifacts.TranscriptKey(run2), p, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("spool file left after an upload")
	}
	p = spool([]byte("kept"))
	srv.Fail = func(*http.Request) int { return http.StatusServiceUnavailable }
	if err := artifacts.PutFile(ctx, remote, artifacts.TranscriptKey(run1), p, 0, ""); err == nil {
		t.Fatal("failed upload reported as success")
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatal("a failed upload removed its spool file")
	}
	srv.Fail = nil
	if err := artifacts.PutFile(ctx, remote, artifacts.TranscriptKey(run1), filepath.Join(tmp, "missing"), 0, ""); err == nil {
		t.Fatal("missing file")
	}
	_ = os.Remove(p)

	// Migrate local → S3: everything copied once, then present; damaged attachment content is refused.
	put(t, local, artifacts.RecordingKey(run1, "seg-000001.smrec.zst"), []byte("seg"))
	put(t, local, artifacts.TranscriptKey(run1), []byte("tr"))
	dry, err := artifacts.Migrate(ctx, local, remote, artifacts.MigrateOptions{TmpDir: tmp, DryRun: true})
	if err != nil || dry.Copied != 3 || dry.Bytes != 4096+3+2 || len(srv.Keys(bucket)) != 1 {
		t.Fatalf("dry run %+v %v", dry, err)
	}
	var seen []string
	rep, err := artifacts.Migrate(ctx, local, remote, artifacts.MigrateOptions{TmpDir: tmp, Progress: func(k string, _ bool) { seen = append(seen, k) }})
	if err != nil || rep.Copied != 3 || rep.Present != 0 || len(seen) != 3 {
		t.Fatalf("migrate %+v %v", rep, err)
	}
	if got := readAll(t, remote, artifacts.AttachmentKey(sum(data))); !bytes.Equal(got, data) {
		t.Fatal("migrated attachment differs")
	}
	rep, err = artifacts.Migrate(ctx, local, remote, artifacts.MigrateOptions{TmpDir: tmp, RemoveSource: true})
	if err != nil || rep.Copied != 0 || rep.Present != 3 || rep.Removed != 3 {
		t.Fatalf("second migrate %+v %v", rep, err)
	}
	if ls, _ := local.List(ctx, ""); len(ls) != 0 {
		t.Fatalf("source not emptied: %+v", ls)
	}
	// And back: S3 → local.
	rep, err = artifacts.Migrate(ctx, remote, local, artifacts.MigrateOptions{TmpDir: tmp})
	if err != nil || rep.Copied != 4 {
		t.Fatalf("migrate back %+v %v", rep, err)
	}
	srv.SetObject(bucket, "sm/art/"+artifacts.AttachmentKey(sum([]byte("claimed"))), []byte("not the claimed content"))
	if _, err := artifacts.Migrate(ctx, remote, local, artifacts.MigrateOptions{TmpDir: tmp}); !errors.Is(err, artifacts.ErrChecksum) {
		t.Fatalf("damaged attachment migrated: %v", err)
	}
	if _, err := artifacts.Migrate(ctx, local, local, artifacts.MigrateOptions{TmpDir: tmp}); err == nil {
		t.Fatal("migration onto itself")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := artifacts.Migrate(cctx, local, remote, artifacts.MigrateOptions{TmpDir: tmp}); err == nil {
		t.Fatal("cancelled migration")
	}
}

func TestKeys(t *testing.T) {
	s := sum([]byte("x"))
	for key, ok := range map[string]bool{
		artifacts.AttachmentKey(s):                                  true,
		"attachments/sha256/00/" + s:                                false, // wrong fan-out directory
		"attachments/sha256/" + s[:2] + "/" + s[:63]:                false,
		artifacts.RecordingKey(run1, "seg-000001.smrec.zst"):        true,
		artifacts.RecordingKey(run1, "seg-1.smrec.zst"):             false,
		artifacts.RecordingKey("run_short", "seg-000001.smrec.zst"): false,
		artifacts.TranscriptKey(run1):                               true,
		artifacts.TranscriptKey(run1) + ".part":                     false,
		"../" + artifacts.TranscriptKey(run1):                       false,
	} {
		if artifacts.ValidKey(key) != ok {
			t.Errorf("ValidKey(%q) = %v", key, !ok)
		}
	}
	if got, ok := artifacts.AttachmentSum(artifacts.AttachmentKey(s)); !ok || got != s {
		t.Fatal("attachment sum")
	}
	if _, ok := artifacts.AttachmentSum(artifacts.TranscriptKey(run1)); ok {
		t.Fatal("not an attachment")
	}
	for p, ok := range map[string]bool{"": true, "spaceman/": true, "a/b-c_d.e/": true, "/x/": false, "x": false, "a//b/": false, "a/../": false, "a b/": false,
		strings.Repeat("a", 300) + "/": false} {
		if (artifacts.ValidatePrefix(p) == nil) != ok {
			t.Errorf("ValidatePrefix(%q)", p)
		}
	}
}

func TestFromConfig(t *testing.T) {
	dir := t.TempDir()
	c := config.Default()
	c.DataDir = dir
	s, err := artifacts.FromConfig(c)
	if err != nil || s.Kind() != "local" {
		t.Fatalf("default %v %v", s, err)
	}
	if l := artifacts.LocalFromConfig(c); l.Root(artifacts.NSAttachments) != filepath.Join(dir, "artifacts") {
		t.Fatalf("attachments dir %q", l.Root(artifacts.NSAttachments))
	}
	c.Artifacts.LocalDir = filepath.Join(dir, "elsewhere")
	if l := artifacts.LocalFromConfig(c); l.Root(artifacts.NSAttachments) != c.Artifacts.LocalDir {
		t.Fatal("artifacts.local_dir ignored")
	}
	srv := s3test.New(t, "AKIDTEST")
	write := func(name, v string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	c.Artifacts.Backend = "s3"
	c.Artifacts.S3 = config.S3Config{Endpoint: srv.URL, Bucket: bucket, Prefix: "p/", AccessKeyFile: write("ak", "AKIDTEST\n"), SecretKeyFile: write("sk", secretValue),
		UsePathStyle: true, AllowPrivateNetwork: true, CAFile: write("ca.pem", srv.CAPEM())}
	s, err = artifacts.FromConfig(c)
	if err != nil || s.Kind() != "s3" {
		t.Fatalf("s3 %v", err)
	}
	put(t, s, artifacts.TranscriptKey(run1), []byte("works"))
	if _, ok := srv.Object(bucket, "p/"+artifacts.TranscriptKey(run1)); !ok {
		t.Fatal("not stored under the prefix")
	}
	for name, mod := range map[string]func(*config.Config){
		"backend":    func(c *config.Config) { c.Artifacts.Backend = "nfs" },
		"bucket":     func(c *config.Config) { c.Artifacts.S3.Bucket = "" },
		"prefix":     func(c *config.Config) { c.Artifacts.S3.Prefix = "/abs" },
		"access key": func(c *config.Config) { c.Artifacts.S3.AccessKeyFile = filepath.Join(dir, "missing") },
		"empty key":  func(c *config.Config) { c.Artifacts.S3.SecretKeyFile = write("empty", " \n") },
		"dir key":    func(c *config.Config) { c.Artifacts.S3.SecretKeyFile = dir },
		"big key":    func(c *config.Config) { c.Artifacts.S3.SecretKeyFile = write("big", strings.Repeat("k", 5000)) },
		"unset key":  func(c *config.Config) { c.Artifacts.S3.SecretKeyFile = "" },
		"ca":         func(c *config.Config) { c.Artifacts.S3.CAFile = filepath.Join(dir, "missing.pem") },
		"endpoint":   func(c *config.Config) { c.Artifacts.S3.Endpoint = "ftp://x" },
	} {
		cc := *c
		mod(&cc)
		_, err := artifacts.FromConfig(&cc)
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if strings.Contains(err.Error(), secretValue) || strings.Contains(err.Error(), "AKIDTEST") {
			t.Errorf("%s: an error carries a credential: %v", name, err)
		}
	}
}

// secretValue is generated per test binary run and must never appear in an error or a location.
var secretValue = "sk-" + hex.EncodeToString([]byte(time.Now().String()))[:24]
