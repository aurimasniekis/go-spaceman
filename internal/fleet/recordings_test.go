package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/proto"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

const recRun = "run_0123456789abcdefghijklmnop"

func chunk(t *testing.T, recs ...recording.Record) proto.RecordingChunk {
	t.Helper()
	c := proto.RecordingChunk{RunID: recRun}
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		c.Records = append(c.Records, b)
	}
	return c
}

func out(seq uint64, s string) recording.Record {
	return recording.Record{Seq: seq, T: int64(seq), Kind: recording.KindOut, Data: []byte(s)}
}

func text(t *testing.T, r *Recordings) string {
	t.Helper()
	res, err := r.Read(context.Background(), recRun, 0)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, rec := range res.Records {
		b.Write(rec.Data)
	}
	return b.String()
}

// SM-STORE-002, ADR-0048: central's recordings with the S3 artifact store. A recording is spooled while it is shipped
// and archived when it closes; it reads the same before, during and after; a node re-shipping its tail after the
// archive is deduplicated, and new records continue in a new segment numbered after the archived ones; a failed
// archive keeps the spool, and ArchiveSpooled retries it; Delete removes both places.
func TestRecordingsArchiveToS3(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "rec", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := newRecordings(dir, artifacts.NewS3(cl, "rec", ""), nil, time.Now)
	defer r.Close()
	ctx := context.Background()
	if seq, err := r.apply(chunk(t, recording.Record{Seq: 1, Kind: recording.KindOpen, Cols: 80, Rows: 24}, out(2, "hello "))); err != nil || seq != 2 {
		t.Fatalf("apply %d %v", seq, err)
	}
	if got := text(t, r); got != "hello " {
		t.Fatalf("spooled recording reads %q", got)
	}
	if _, err := r.apply(chunk(t, out(3, "world"), recording.Record{Seq: 4, Kind: recording.KindClose})); err != nil {
		t.Fatal(err)
	}
	r.wg.Wait()
	if _, err := os.Stat(filepath.Join(dir, recRun)); !os.IsNotExist(err) {
		t.Fatalf("spool left after the archive: %v", err)
	}
	if keys := srv.Keys("rec"); len(keys) != 1 || keys[0] != artifacts.RecordingKey(recRun, recording.SegmentName(1)) {
		t.Fatalf("bucket %v", keys)
	}
	if got := text(t, r); got != "hello world" {
		t.Fatalf("archived recording reads %q", got)
	}
	names, err := r.Segments(ctx, recRun)
	if err != nil || len(names) != 1 {
		t.Fatalf("segments %v %v", names, err)
	}
	seg, err := r.OpenSegment(ctx, recRun, names[0])
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(seg); len(b) == 0 {
		t.Fatal("empty archived segment")
	}
	_ = seg.Close()

	// The node re-ships its tail (it never saw the acknowledgement) and then more output.
	if seq, err := r.apply(chunk(t, out(3, "world"), recording.Record{Seq: 4, Kind: recording.KindClose})); err != nil || seq != 4 {
		t.Fatalf("re-shipped tail: %d %v", seq, err)
	}
	if seq, err := r.apply(chunk(t, out(5, "!"))); err != nil || seq != 5 {
		t.Fatalf("after the archive: %d %v", seq, err)
	}
	if _, err := os.Stat(filepath.Join(dir, recRun, recording.SegmentName(2))); err != nil {
		t.Fatalf("new records did not continue in segment 2: %v", err)
	}
	if got := text(t, r); got != "hello world!" {
		t.Fatalf("merged recording reads %q", got)
	}

	// The run ends without a close record while the bucket is down: the spool stays and is archived later.
	srv.Fail = func(*http.Request) int { return http.StatusServiceUnavailable }
	r.RunEnded(recRun)
	r.wg.Wait()
	if _, err := os.Stat(filepath.Join(dir, recRun, recording.SegmentName(2))); err != nil {
		t.Fatalf("a failed archive removed the spool: %v", err)
	}
	if _, err := r.Read(ctx, recRun, 0); err == nil {
		t.Fatal("reading without the bucket succeeded")
	}
	srv.Fail = nil
	n, err := r.ArchiveSpooled(ctx, func(context.Context, string) bool { return true })
	if err != nil || n != 1 || len(srv.Keys("rec")) != 2 {
		t.Fatalf("archive spooled: %d %v %v", n, err, srv.Keys("rec"))
	}
	if got := text(t, r); got != "hello world!" {
		t.Fatalf("after the retry %q", got)
	}
	if n, _ := r.ArchiveSpooled(ctx, func(context.Context, string) bool { return false }); n != 0 {
		t.Fatal("archived a run that has not ended")
	}

	if err := r.Delete(ctx, recRun); err != nil {
		t.Fatal(err)
	}
	if keys := srv.Keys("rec"); len(keys) != 0 {
		t.Fatalf("left in the bucket: %v", keys)
	}
	if text(t, r) != "" {
		t.Fatal("deleted recording reads")
	}
	if _, err := r.OpenSegment(ctx, recRun, "notes.txt"); err == nil {
		t.Fatal("non-segment opened")
	}
	if _, err := r.Read(ctx, "../x", 0); err == nil {
		t.Fatal("invalid run read")
	}
	if err := r.Archive(ctx, "../x"); err == nil {
		t.Fatal("invalid run archived")
	}
}

// With the local store the run folder is the store: nothing is archived or uploaded.
func TestRecordingsLocalStoreStaysInPlace(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, time.Now)
	defer r.Close()
	if r.archive {
		t.Fatal("the local store archives")
	}
	if _, err := r.apply(chunk(t, out(1, "local"), recording.Record{Seq: 2, Kind: recording.KindClose})); err != nil {
		t.Fatal(err)
	}
	r.RunEnded(recRun)
	if err := r.Archive(context.Background(), recRun); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, recRun, recording.SegmentName(1))); err != nil {
		t.Fatalf("local recording moved: %v", err)
	}
	if n, err := r.ArchiveSpooled(context.Background(), func(context.Context, string) bool { return true }); n != 0 || err != nil {
		t.Fatalf("archive spooled %d %v", n, err)
	}
	if text(t, r) != "local" {
		t.Fatal("local recording")
	}
	// A local store rooted elsewhere is a separate store: recordings are archived into it.
	other := t.TempDir()
	r2 := newRecordings(dir, artifacts.NewLocal(map[string]string{artifacts.NSRecordings: other}), nil, time.Now)
	defer r2.Close()
	if err := r2.Archive(context.Background(), recRun); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, recRun, recording.SegmentName(1))); err != nil {
		t.Fatalf("archived into the other store: %v", err)
	}
	if text(t, r2) != "local" {
		t.Fatal("archived local recording")
	}
}

// SM-UI-004, SM-AUTH-004: a live viewer only ever receives its own run's records. Viewers of two runs subscribe side by
// side; each run's chunks reach only that run's viewers, and catch-up reads only that run's recording, so one run's
// terminal can never be delivered on another run's stream (the web terminal reconnects per run, never per page).
func TestRecordingsViewersAreScopedByRun(t *testing.T) {
	r := newRecordings(t.TempDir(), nil, nil, time.Now)
	defer r.Close()
	const runB = "run_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	va, vb := r.Subscribe(recRun), r.Subscribe(runB)
	defer r.Unsubscribe(va)
	defer r.Unsubscribe(vb)
	if _, err := r.apply(chunk(t, out(1, "secret of A"))); err != nil {
		t.Fatal(err)
	}
	cb := chunk(t, out(1, "output of B"))
	cb.RunID = runB
	if _, err := r.apply(cb); err != nil {
		t.Fatal(err)
	}
	got := func(v *Viewer) []string {
		var s []string
		for {
			select {
			case rec := <-v.C:
				s = append(s, string(rec.Data))
			default:
				return s
			}
		}
	}
	if a, b := got(va), got(vb); len(a) != 1 || a[0] != "secret of A" || len(b) != 1 || b[0] != "output of B" {
		t.Fatalf("viewer A got %q, viewer B got %q", a, b)
	}
	res, err := r.Read(context.Background(), runB, 0)
	if err != nil || len(res.Records) != 1 || string(res.Records[0].Data) != "output of B" {
		t.Fatalf("catch-up of B: %+v %v", res.Records, err)
	}
	// Unsubscribing one run's last viewer leaves the other run's viewers in place.
	r.Unsubscribe(va)
	if _, err := r.apply(chunk(t, out(2, "more of A"))); err != nil {
		t.Fatal(err)
	}
	if b := got(vb); len(b) != 0 {
		t.Fatalf("viewer B got A's output %q", b)
	}
}
