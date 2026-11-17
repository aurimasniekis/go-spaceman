package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
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
	if seq, err := r.apply(1, chunk(t, recording.Record{Seq: 1, Kind: recording.KindOpen, Cols: 80, Rows: 24}, out(2, "hello "))); err != nil || seq != 2 {
		t.Fatalf("apply %d %v", seq, err)
	}
	if got := text(t, r); got != "hello " {
		t.Fatalf("spooled recording reads %q", got)
	}
	if _, err := r.apply(1, chunk(t, out(3, "world"), recording.Record{Seq: 4, Kind: recording.KindClose})); err != nil {
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
	if seq, err := r.apply(1, chunk(t, out(3, "world"), recording.Record{Seq: 4, Kind: recording.KindClose})); err != nil || seq != 4 {
		t.Fatalf("re-shipped tail: %d %v", seq, err)
	}
	if seq, err := r.apply(1, chunk(t, out(5, "!"))); err != nil || seq != 5 {
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
	if _, err := r.apply(1, chunk(t, out(1, "local"), recording.Record{Seq: 2, Kind: recording.KindClose})); err != nil {
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
	if _, err := r.apply(1, chunk(t, out(1, "secret of A"))); err != nil {
		t.Fatal(err)
	}
	cb := chunk(t, out(1, "output of B"))
	cb.RunID = runB
	if _, err := r.apply(1, cb); err != nil {
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
	if _, err := r.apply(1, chunk(t, out(2, "more of A"))); err != nil {
		t.Fatal(err)
	}
	if b := got(vb); len(b) != 0 {
		t.Fatalf("viewer B got A's output %q", b)
	}
}

// SM-REC-004: a viewer's lagged flag is set by apply and read by the viewer's own goroutine; under -race this must
// not be a data race.
func TestRecordingsViewerLaggedNoRace(t *testing.T) {
	r := newRecordings(t.TempDir(), nil, nil, time.Now)
	defer r.Close()
	v := r.Subscribe(recRun)
	defer r.Unsubscribe(v)
	done := make(chan error, 1)
	go func() {
		for seq := uint64(1); seq <= 600; seq++ {
			if _, err := r.apply(1, chunk(t, out(seq, "x"))); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	took := false
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			if !took && !v.TakeLagged() {
				t.Fatal("600 records into a 512-record queue: viewer never lagged")
			}
			return
		default:
			if v.Lagged() && v.TakeLagged() {
				took = true
			}
		}
	}
}

// SM-REC-004: a viewer that lagged catches up from storage once, then streams live records again. The loop below is
// the terminal stream's (internal/api terminalStream): it re-reads storage when the viewer lagged or a sequence is
// missing. Before the flag was cleared, every live record after one lag triggered a full re-read of the recording.
func TestRecordingsLaggedViewerCatchesUpOnce(t *testing.T) {
	r := newRecordings(t.TempDir(), nil, nil, time.Now)
	defer r.Close()
	v := r.Subscribe(recRun)
	defer r.Unsubscribe(v)
	var after uint64
	catchUps := 0
	catchUp := func() {
		catchUps++
		res, err := r.Read(context.Background(), recRun, after)
		if err != nil {
			t.Fatal(err)
		}
		for _, rec := range res.Records {
			after = rec.Seq
		}
	}
	consume := func() {
		for {
			select {
			case rec := <-v.C:
				if rec.Seq <= after {
					continue
				}
				if v.TakeLagged() || rec.Seq != after+1 {
					catchUp()
					continue
				}
				after = rec.Seq
			default:
				return
			}
		}
	}
	// Overflow the viewer's queue: records past its capacity are dropped and the viewer is marked lagged.
	const overflow = 600
	for seq := uint64(1); seq <= overflow; seq++ {
		if _, err := r.apply(1, chunk(t, out(seq, "x"))); err != nil {
			t.Fatal(err)
		}
	}
	if !v.Lagged() {
		t.Fatal("viewer not lagged after overflowing its queue")
	}
	consume()
	if catchUps != 1 || after != overflow || v.Lagged() {
		t.Fatalf("after the lag: %d catch-ups (want 1), at seq %d (want %d), lagged %v", catchUps, after, overflow, v.Lagged())
	}
	// Live again: each new record is delivered in order with no further catch-up.
	for seq := uint64(overflow + 1); seq <= overflow+50; seq++ {
		if _, err := r.apply(1, chunk(t, out(seq, "y"))); err != nil {
			t.Fatal(err)
		}
		consume()
	}
	if catchUps != 1 || after != overflow+50 {
		t.Fatalf("live records after the catch-up: %d catch-ups (want 1), at seq %d (want %d)", catchUps, after, overflow+50)
	}
}

type syncFailFile struct {
	recording.File
	fail *atomic.Bool
}

func (f syncFailFile) Sync() error {
	if f.fail.Load() {
		return syscall.EIO
	}
	return f.File.Sync()
}

// A failed flush is never acked: the ack carries only what is durably stored, so the node keeps (and re-ships) the
// rest. The failed writer is dropped, the next chunk reopens the recording, and the re-shipped records fill the gap.
func TestRecordingsAckOnlyStoredAndReopenAfterFailure(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, time.Now)
	defer r.Close()
	var fail atomic.Bool
	r.create = func(path string) (recording.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		return syncFailFile{File: f, fail: &fail}, nil
	}
	if seq, err := r.apply(1, chunk(t, out(1, "a"), out(2, "b"))); err != nil || seq != 2 {
		t.Fatalf("first chunk acked %d %v", seq, err)
	}
	fail.Store(true)
	seq, err := r.apply(1, chunk(t, out(3, "c")))
	if err == nil {
		t.Fatal("failing flush returned no error")
	}
	if seq != 2 {
		t.Fatalf("ack %d covers the failed chunk, want 2", seq)
	}
	if r.w[recRun] != nil {
		t.Fatal("failed writer kept")
	}
	// The disk recovers: the node re-ships from its ack, and everything is stored.
	fail.Store(false)
	seq, err = r.apply(1, chunk(t, out(3, "c"), out(4, "d")))
	if err != nil || seq != 4 {
		t.Fatalf("after recovery acked %d %v, want 4", seq, err)
	}
	if got := text(t, r); got != "abcd" {
		t.Fatalf("recording %q, want abcd", got)
	}
}

// failingRecordings is central's recordings whose segment files fail Sync while fail is set.
func failingRecordings(t *testing.T) (*Recordings, *atomic.Bool) {
	t.Helper()
	r := newRecordings(t.TempDir(), nil, nil, time.Now)
	t.Cleanup(r.Close)
	var fail atomic.Bool
	r.create = func(path string) (recording.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // test path
		if err != nil {
			return nil, err
		}
		return syncFailFile{File: f, fail: &fail}, nil
	}
	return r, &fail
}

// SM-REC-003 (ADR-0010 addendum): after central failed to store a chunk, the next live chunk on the same connection
// (sent behind the lost one) is not stored past the hole and is acked only up to it, so the node keeps the hole's
// records; once the node re-ships on its next connection, central holds the whole recording, contiguous.
func TestRecordingsStoreNothingPastTheGapAfterAFailure(t *testing.T) {
	r, fail := failingRecordings(t)
	if seq, err := r.apply(1, chunk(t, out(1, "a"), out(2, "b"))); err != nil || seq != 2 {
		t.Fatalf("first chunk acked %d %v", seq, err)
	}
	fail.Store(true)
	if seq, err := r.apply(1, chunk(t, out(3, "c"))); err == nil || seq != 2 {
		t.Fatalf("failed chunk acked %d %v, want 2 and an error", seq, err)
	}
	fail.Store(false)
	// The next live chunk, already on its way on the same connection.
	for range 2 {
		seq, err := r.apply(1, chunk(t, out(4, "d"), out(5, "e")))
		if err != nil || seq != 2 {
			t.Fatalf("live chunk past the hole acked %d %v, want 2", seq, err)
		}
		if got := text(t, r); got != "ab" {
			t.Fatalf("central stored %q past the hole, want ab", got)
		}
	}
	// The node's catch-up on its next connection re-ships from its acknowledgement.
	seq, err := r.apply(2, chunk(t, out(3, "c"), out(4, "d"), out(5, "e")))
	if err != nil || seq != 5 {
		t.Fatalf("re-ship acked %d %v, want 5", seq, err)
	}
	res, err := r.Read(context.Background(), recRun, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := text(t, r); got != "abcde" || res.Gaps != 0 {
		t.Fatalf("central holds %q with %d gaps, want abcde and none", got, res.Gaps)
	}
	if len(r.resync) != 0 {
		t.Fatalf("resync marks left: %v", r.resync)
	}
}

// A chunk on the failed connection that starts right after what is stored fills the hole and ends the resync; on a
// new connection a jump is the node's own (records it never had) and is kept, as for any recording.
func TestRecordingsResyncEndsOnAContiguousChunkOrANewConnection(t *testing.T) {
	r, fail := failingRecordings(t)
	if _, err := r.apply(1, chunk(t, out(1, "a"))); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	if _, err := r.apply(1, chunk(t, out(2, "b"))); err == nil {
		t.Fatal("failing chunk stored")
	}
	fail.Store(false)
	if seq, err := r.apply(1, chunk(t, out(1, "a"))); err != nil || seq != 1 {
		t.Fatalf("repeat acked %d %v", seq, err)
	}
	if seq, err := r.apply(1, chunk(t, out(2, "b"), out(3, "c"))); err != nil || seq != 3 {
		t.Fatalf("contiguous chunk acked %d %v, want 3", seq, err)
	}
	if seq, err := r.apply(1, chunk(t, out(5, "e"))); err != nil || seq != 5 {
		t.Fatalf("after the resync a jump acked %d %v, want 5 (kept as a gap)", seq, err)
	}

	fail.Store(true)
	if _, err := r.apply(1, chunk(t, out(6, "f"))); err == nil {
		t.Fatal("failing chunk stored")
	}
	fail.Store(false)
	// The node's own recording lacks 6 (its disk failed too): its catch-up on the new connection starts at 7.
	if seq, err := r.apply(2, chunk(t, out(7, "g"))); err != nil || seq != 7 {
		t.Fatalf("catch-up on a new connection acked %d %v, want 7", seq, err)
	}
	if got := text(t, r); got != "abceg" {
		t.Fatalf("central holds %q, want abceg", got)
	}
}

// SM-REC-003: a chunk whose sync failed is not acked by the writer reopened after the failure, although its frame
// may still decode from the page cache: the failure cut it off, so the next chunk is not acked past it either.
func TestRecordingsReopenedWriterDoesNotTrustAnUnsyncedTail(t *testing.T) {
	r, fail := failingRecordings(t)
	if seq, err := r.apply(1, chunk(t, out(1, "a"))); err != nil || seq != 1 {
		t.Fatalf("first chunk acked %d %v", seq, err)
	}
	fail.Store(true)
	if seq, err := r.apply(1, chunk(t, out(2, "b"))); err == nil || seq != 1 {
		t.Fatalf("chunk with a failed sync acked %d %v, want 1 and an error", seq, err)
	}
	fail.Store(false)
	if seq, err := r.apply(1, chunk(t, out(3, "c"))); err != nil || seq != 1 {
		t.Fatalf("next chunk acked %d %v, want 1: the unsynced record 2 is not stored", seq, err)
	}
	if got := text(t, r); got != "a" {
		t.Fatalf("central holds %q, want a", got)
	}
	if seq, err := r.apply(2, chunk(t, out(2, "b"), out(3, "c"))); err != nil || seq != 3 {
		t.Fatalf("re-ship acked %d %v, want 3", seq, err)
	}
	if got := text(t, r); got != "abc" {
		t.Fatalf("central holds %q, want abc", got)
	}
}

// When the unsynced tail of a failed segment cannot be cut off, central does not reopen the recording (it would read
// that tail as stored): every chunk fails until the cut succeeds.
func TestRecordingsDoNotReopenUntilTheUnsyncedTailIsCut(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	r, fail := failingRecordings(t)
	if _, err := r.apply(1, chunk(t, out(1, "a"))); err != nil {
		t.Fatal(err)
	}
	dir, _ := r.Dir(recRun)
	seg := filepath.Join(dir, recording.SegmentName(1))
	fail.Store(true)
	if err := os.Chmod(seg, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := r.apply(1, chunk(t, out(2, "b"))); err == nil {
		t.Fatal("failing chunk stored")
	}
	fail.Store(false)
	if seq, err := r.apply(2, chunk(t, out(2, "b"))); err == nil || seq != 0 {
		t.Fatalf("chunk while the tail could not be cut acked %d %v, want an error", seq, err)
	}
	if err := os.Chmod(seg, 0o600); err != nil {
		t.Fatal(err)
	}
	if seq, err := r.apply(2, chunk(t, out(2, "b"))); err != nil || seq != 2 {
		t.Fatalf("after the cut acked %d %v, want 2", seq, err)
	}
	if got := text(t, r); got != "ab" {
		t.Fatalf("central holds %q, want ab", got)
	}
}
