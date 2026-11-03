package attachments

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

const testBucket = "sm-att"

func s3Blobs(t *testing.T) (*s3test.Server, func(dir string) Blobs) {
	t.Helper()
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: testBucket, PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	return srv, func(dir string) Blobs {
		return NewStore(artifacts.NewS3(cl, testBucket, "a/"), filepath.Join(dir, "spool"))
	}
}

// SM-STORE-002: attachments in S3. Content is addressed by its SHA-256 under attachments/sha256/, uploads stream
// through a spool file that is removed afterwards, identical content is stored once, the space quota is the same,
// downloads stream with range requests, damaged content fails its download, and garbage collection deletes from
// the bucket.
func TestS3Attachments(t *testing.T) {
	srv, mk := s3Blobs(t)
	f := newFxWith(t, Options{MaxBytes: 2 << 20, QuotaBytes: 3 << 20}, mk)
	data := make([]byte, 1<<20)
	_, _ = rand.Read(data)
	a, err := f.svc.Upload(f.ctx, actor, "spc_a", "blob.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	key := "a/attachments/sha256/" + a.SHA256[:2] + "/" + a.SHA256
	if got, ok := srv.Object(testBucket, key); !ok || !bytes.Equal(got, data) {
		t.Fatalf("bucket keys %v", srv.Keys(testBucket))
	}
	if ents, _ := os.ReadDir(filepath.Join(f.blob, "spool")); len(ents) != 0 {
		t.Fatalf("spool files left: %d", len(ents))
	}
	puts := srv.Requests
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "again.bin", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if srv.Requests != puts+1 { // one HEAD: the content is fresh, so it is not uploaded again
		t.Fatalf("a duplicate upload made %d requests", srv.Requests-puts)
	}
	// Quota accounting is the database's and does not depend on the backend.
	if _, err := f.svc.Upload(f.ctx, actor, "spc_a", "third.bin", bytes.NewReader(make([]byte, 1<<20+1))); code(err) != "precondition_failed" {
		t.Fatalf("quota: %v", err)
	}

	// Download through net/http like the API: whole, and a range.
	_, rc, err := f.svc.Open(f.ctx, "spc_a", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeContent(w, r, "", time.Time{}, rc) }))
	defer h.Close()
	req, _ := http.NewRequest(http.MethodGet, h.URL, nil)
	req.Header.Set("Range", "bytes=1000-1999")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusPartialContent || !bytes.Equal(part, data[1000:2000]) || srv.Ranges == 0 {
		t.Fatalf("range download %d (%d bytes, %d range requests)", res.StatusCode, len(part), srv.Ranges)
	}
	res, err = http.Get(h.URL) //nolint:noctx // test
	if err != nil {
		t.Fatal(err)
	}
	whole, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if !bytes.Equal(whole, data) {
		t.Fatal("whole download differs")
	}
	_ = rc.Close()

	// Damaged content in the bucket: a full read fails instead of returning it.
	srv.SetObject(testBucket, key, append([]byte{data[0] ^ 1}, data[1:]...))
	_, rc, err = f.svc.Open(f.ctx, "spc_a", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); !errors.Is(err, artifacts.ErrChecksum) {
		t.Fatalf("damaged content read: %v", err)
	}
	_ = rc.Close()
	srv.SetObject(testBucket, key, data)

	// Garbage collection deletes unreferenced content from the bucket (once it is older than an hour).
	small, err := f.svc.Upload(f.ctx, actor, "spc_b", "small.txt", strings.NewReader("small"))
	if err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(UnreferencedGrace + time.Minute)
	for _, k := range srv.Keys(testBucket) {
		srv.SetModTime(testBucket, k, time.Now().Add(-2*time.Hour))
	}
	rows, blobs, err := f.svc.GC(f.ctx)
	if err != nil || rows != 3 || blobs != 3 { // the upload refused by the quota left its content too
		t.Fatalf("gc rows=%d blobs=%d %v", rows, blobs, err)
	}
	if keys := srv.Keys(testBucket); len(keys) != 0 {
		t.Fatalf("left in the bucket: %v", keys)
	}
	_ = small

	// Stored content older than the refresh age is uploaded again, so a concurrent collection cannot remove it.
	b := mk(t.TempDir())
	sum, _, _, err := b.Write(f.ctx, strings.NewReader("refresh"), 100)
	if err != nil {
		t.Fatal(err)
	}
	srv.SetModTime(testBucket, "a/"+artifacts.AttachmentKey(sum), time.Now().Add(-time.Hour))
	if _, _, _, err := b.Write(f.ctx, strings.NewReader("refresh"), 100); err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(f.ctx, sum, time.Now().Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if sums, _ := b.List(f.ctx); len(sums) != 1 {
		t.Fatal("refreshed content was collected")
	}
	if err := b.Remove(f.ctx, "nope", time.Now()); err == nil {
		t.Fatal("invalid sum")
	}
	if err := b.Remove(f.ctx, strings.Repeat("0", 64), time.Now()); err != nil {
		t.Fatalf("removing missing content: %v", err)
	}
	if _, err := b.Open(f.ctx, "nope"); err == nil {
		t.Fatal("invalid sum opened")
	}
	srv.Fail = func(*http.Request) int { return http.StatusServiceUnavailable }
	if _, _, _, err := b.Write(f.ctx, strings.NewReader("down"), 100); err == nil {
		t.Fatal("upload to an unavailable bucket succeeded")
	}
	if err := b.Remove(f.ctx, sum, time.Now()); err == nil {
		t.Fatal("remove error hidden")
	}
	if _, err := b.List(f.ctx); err == nil {
		t.Fatal("list error hidden")
	}
}
