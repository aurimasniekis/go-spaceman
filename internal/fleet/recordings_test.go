package fleet

import (
	"context"
	"encoding/json"
	"errors"
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
	r.reopenBackoff = 0
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
	r.reopenBackoff = 0
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
	if seq, err := r.apply(2, chunk(t, out(2, "b"))); err == nil || seq != 1 {
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

// Finding 3 of the PR 242 review: on a disk that keeps failing, chunks are refused without reopening the recording (a
// read of the whole recording under the global lock) until the backoff ends, no empty segment is left behind, and a
// refused chunk is still acked with what is stored (the hub then closes the connection, or the node never re-ships).
func TestRecordingsReopenBackoffOnAFailingDisk(t *testing.T) {
	var clock atomic.Int64
	base := time.Now()
	now := func() time.Time { return base.Add(time.Duration(clock.Load())) }
	r := newRecordings(t.TempDir(), nil, nil, now)
	t.Cleanup(r.Close)
	var fail atomic.Bool
	var creates atomic.Int32
	r.create = func(path string) (recording.File, error) {
		creates.Add(1)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // test path
		if err != nil {
			return nil, err
		}
		return syncFailFile{File: f, fail: &fail}, nil
	}
	if seq, err := r.apply(1, chunk(t, out(1, "a"))); err != nil || seq != 1 {
		t.Fatalf("first chunk acked %d %v", seq, err)
	}
	dir, _ := r.Dir(recRun)
	segs := func() int {
		s, _ := recording.Segments(dir)
		return len(s)
	}
	fail.Store(true)
	// The first failure and the 10 chunks inside the backoff window: each is refused with the stored sequence.
	for i := range 11 {
		seq, err := r.apply(1, chunk(t, out(uint64(2+i), "x")))
		if err == nil || seq != 1 {
			t.Fatalf("chunk %d acked %d %v, want 1 and an error", i, seq, err)
		}
	}
	if n := creates.Load(); n != 1 { // only segment 1: the first failure hit its open file
		t.Fatalf("%d segment files created, want 1: refused chunks reopened the recording", n)
	}
	if n := segs(); n != 1 {
		t.Fatalf("%d segments after the failures, want 1 (no empty or partial one left)", n)
	}
	// After the window one reopen is tried per window.
	for window := range 3 {
		clock.Add(int64(r.reopenBackoff + time.Second))
		before := creates.Load()
		for i := range 10 {
			if _, err := r.apply(uint64(window+2), chunk(t, out(uint64(2+i), "y"))); err == nil {
				t.Fatal("failing chunk stored")
			}
		}
		if got := creates.Load() - before; got != 1 {
			t.Fatalf("window %d: %d reopens (segment creations), want 1", window, got)
		}
		if n := segs(); n != 1 {
			t.Fatalf("window %d: %d segments, want 1", window, n)
		}
	}
	// The disk recovers; after the window the node's re-ship is stored and the backoff is gone.
	fail.Store(false)
	clock.Add(int64(r.reopenBackoff + time.Second))
	if seq, err := r.apply(2, chunk(t, out(2, "b"))); err != nil || seq != 2 {
		t.Fatalf("after recovery acked %d %v, want 2", seq, err)
	}
	if len(r.failed) != 0 {
		t.Fatalf("failure marks left: %v", r.failed)
	}
}

// A failed segment that never held a synced frame is removed, not left empty.
func TestRecordingsFailedFirstSegmentIsRemoved(t *testing.T) {
	r, fail := failingRecordings(t)
	fail.Store(true)
	if _, err := r.apply(1, chunk(t, out(1, "a"))); err == nil {
		t.Fatal("failing chunk stored")
	}
	dir, _ := r.Dir(recRun)
	if segs, _ := recording.Segments(dir); len(segs) != 0 {
		t.Fatalf("segments left: %v", segs)
	}
}

// SM-REC-003 (M8): a record that fills the writer's buffer is flushed inside Append; when that flush fails, apply
// drops the writer, acks only what was stored, marks the run for a resync and starts the reopen backoff.
func TestRecordingsAppendFlushFailureIsHandled(t *testing.T) {
	r, fail := failingRecordings(t)
	r.reopenBackoff = time.Hour
	if seq, err := r.apply(1, chunk(t, out(1, "a"))); err != nil || seq != 1 {
		t.Fatalf("first chunk acked %d %v", seq, err)
	}
	fail.Store(true)
	big := out(2, strings.Repeat("x", 80<<10)) // above the 64 KiB flush threshold: Append itself flushes
	seq, err := r.apply(1, chunk(t, big, out(3, "c")))
	if err == nil || seq != 1 {
		t.Fatalf("failing Append acked %d %v, want 1 and an error", seq, err)
	}
	if r.w[recRun] != nil {
		t.Fatal("failed writer kept")
	}
	if r.resync[recRun] != 1 {
		t.Fatalf("resync marks %v, want the run marked on connection 1", r.resync)
	}
	if f, ok := r.failed[recRun]; !ok || f.stored != 1 {
		t.Fatalf("failure marks %v, want the run with stored 1", r.failed)
	}
	if seq, err := r.apply(1, chunk(t, out(4, "d"))); !errors.Is(err, errReopenBackoff) || seq != 1 {
		t.Fatalf("chunk in the backoff acked %d %v, want 1 and errReopenBackoff", seq, err)
	}
}

// Resuming an archived recording: with no local spool, the writer continues from the archived tail, and a chunk that
// only repeats archived records is acked with that tail (not 0, which would make the node re-ship everything).
func TestRecordingsArchiveResumeAcksTheArchivedTail(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "rec", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	r := newRecordings(dir, artifacts.NewS3(cl, "rec", ""), nil, time.Now)
	defer r.Close()
	if seq, err := r.apply(1, chunk(t, out(1, "a"), out(2, "b"), out(3, "c"))); err != nil || seq != 3 {
		t.Fatalf("apply %d %v", seq, err)
	}
	r.RunEnded(recRun)
	r.wg.Wait()
	if _, err := os.Stat(filepath.Join(dir, recRun)); !os.IsNotExist(err) {
		t.Fatalf("spool left after the archive: %v", err)
	}
	if seq, err := r.apply(2, chunk(t, out(2, "b"), out(3, "c"))); err != nil || seq != 3 {
		t.Fatalf("repeats of archived records acked %d %v, want 3", seq, err)
	}
	if seq, err := r.apply(2, chunk(t, out(4, "d"))); err != nil || seq != 4 {
		t.Fatalf("continuation acked %d %v, want 4", seq, err)
	}
	if _, err := os.Stat(filepath.Join(dir, recRun, recording.SegmentName(2))); err != nil {
		t.Fatalf("records did not continue in segment 2: %v", err)
	}
}

func isClose(err error) bool {
	var cc *closeConn
	return errors.As(err, &cc)
}

// ADR-0010 addendum: a close of the node connection marks every live run of the node unknown, so central asks for one
// only when a re-ship can help (a storage failure) and at most once per (run, connection): the chunks already on
// their way behind the failed one, refused in the reopen backoff, do not close it again. On a new connection the
// refusal of a chunk that needs a re-ship closes it once more.
func TestRecordingsStorageFailureClosesTheConnectionOncePerRunAndConnection(t *testing.T) {
	var clock atomic.Int64
	base := time.Now()
	r, fail := failingRecordings(t)
	r.now = func() time.Time { return base.Add(time.Duration(clock.Load())) }
	r.reopenBackoff = time.Hour
	if _, err := r.apply(1, chunk(t, out(1, "a"))); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	closes := 0
	for i := range 5 {
		_, err := r.apply(1, chunk(t, out(uint64(2+i), "x")))
		if err == nil {
			t.Fatal("failing chunk stored")
		}
		if isClose(err) {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("%d closes on one connection, want 1", closes)
	}
	// The node reconnects and re-ships while central is still in the backoff: refused, so closed once more.
	closes = 0
	for i := range 3 {
		_, err := r.apply(2, chunk(t, out(2, "b")))
		if !errors.Is(err, errReopenBackoff) {
			t.Fatalf("chunk %d: %v, want errReopenBackoff", i, err)
		}
		if isClose(err) {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("%d closes on the new connection, want 1", closes)
	}
	// Another run is closed independently.
	if !isClose(r.closeLocked(2, "run_aaaaaaaaaaaaaaaaaaaaaaaaaa", errors.New("x"))) {
		t.Fatal("another run's failure did not close")
	}
}

// A recording that cannot be opened (not a storage failure of a chunk) does not close the connection: the chunk is
// acked with what is stored, the node keeps it, and on that connection nothing is stored past the hole.
func TestRecordingsOpenFailureDoesNotCloseTheConnection(t *testing.T) {
	r := newRecordings(t.TempDir(), nil, nil, time.Now)
	r.reopenBackoff = 0
	t.Cleanup(r.Close)
	dir, _ := r.Dir(recRun)
	if err := os.WriteFile(dir, []byte("not a folder"), 0o600); err != nil { // MkdirAll fails
		t.Fatal(err)
	}
	for i := range 4 {
		seq, err := r.apply(1, chunk(t, out(uint64(1+i), "a")))
		if err == nil || isClose(err) || seq != 0 {
			t.Fatalf("chunk %d: acked %d, %v (close %v), want 0 and a plain error", i, seq, err, isClose(err))
		}
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	// The disk is fine again, but the chunks above 1 sent behind the refused ones must not be stored past the hole.
	if seq, err := r.apply(1, chunk(t, out(3, "c"))); err != nil || seq != 0 {
		t.Fatalf("chunk past the hole acked %d %v, want 0", seq, err)
	}
	if seq, err := r.apply(1, chunk(t, out(1, "a"), out(2, "b"), out(3, "c"))); err != nil || seq != 3 {
		t.Fatalf("contiguous chunk acked %d %v, want 3", seq, err)
	}
}

// An open failure alone never closes the connection, not even for the chunks refused in its backoff window.
func TestRecordingsBackoffRefusalOfAnOpenFailureDoesNotClose(t *testing.T) {
	r := newRecordings(t.TempDir(), nil, nil, time.Now)
	r.reopenBackoff = time.Hour
	t.Cleanup(r.Close)
	dir, _ := r.Dir(recRun)
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := r.apply(1, chunk(t, out(1, "a"))); err == nil || isClose(err) {
			t.Fatalf("chunk %d: %v", i, err)
		}
	}
}

// M4: after a failure, a chunk of only repeats does not end the resync: a jumping chunk behind it on the same
// connection is still not stored past the hole.
func TestRecordingsRepeatsDoNotEndTheResync(t *testing.T) {
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
	if seq, err := r.apply(1, chunk(t, out(4, "d"))); err != nil || seq != 1 {
		t.Fatalf("jump after the repeats acked %d %v, want 1 (not stored past the hole)", seq, err)
	}
	if got := text(t, r); got != "a" {
		t.Fatalf("central holds %q, want a", got)
	}
	if _, ok := r.resync[recRun]; !ok {
		t.Fatal("repeats ended the resync")
	}
}

// The marks of a run that ended, or that nobody shipped to for an hour, are dropped; a run with an unsynced tail that
// cannot be cut keeps its entry.
func TestRecordingsMarksArePruned(t *testing.T) {
	var clock atomic.Int64
	base := time.Now()
	r, fail := failingRecordings(t)
	r.now = func() time.Time { return base.Add(time.Duration(clock.Load())) }
	other := "run_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	fail.Store(true)
	for _, run := range []string{recRun, other} {
		c := chunk(t, out(1, "a"))
		c.RunID = run
		if _, err := r.apply(1, c); err == nil {
			t.Fatal("failing chunk stored")
		}
	}
	empty := func() bool {
		return len(r.resync)+len(r.failed)+len(r.closedOn)+len(r.touched)+len(r.unsynced) == 0
	}
	if empty() {
		t.Fatal("no marks after failures")
	}
	r.RunEnded(recRun)
	if _, ok := r.resync[recRun]; ok || len(r.failed) != 1 {
		t.Fatalf("an ended run keeps marks: %v %v", r.resync, r.failed)
	}
	// An hour later another run's chunk sweeps the idle run's marks.
	clock.Add(int64(markMaxAge + markPruneEvery))
	fail.Store(false)
	third := "run_cccccccccccccccccccccccccc"
	c := chunk(t, out(1, "a"))
	c.RunID = third
	if _, err := r.apply(1, c); err != nil {
		t.Fatal(err)
	}
	// The resync marks stay: they guard a live connection and end with it.
	if len(r.failed)+len(r.closedOn)+len(r.touched)+len(r.unsynced) != 0 || len(r.resync) != 1 {
		t.Fatalf("marks left: resync %v failed %v closed %v touched %v unsynced %v", r.resync, r.failed, r.closedOn, r.touched, r.unsynced)
	}
	r.ConnEnded(2) // another connection: nothing to drop
	if len(r.resync) != 1 {
		t.Fatal("another connection's end dropped the mark")
	}
	r.ConnEnded(1)
	if !empty() {
		t.Fatalf("marks left: resync %v failed %v closed %v touched %v unsynced %v", r.resync, r.failed, r.closedOn, r.touched, r.unsynced)
	}
}

// Round 4, HIGH: an open failure's resync mark must outlive the age-based prune while its connection is open, or the
// next chunk on that connection is stored and acked past the gap (the record is lost for good).
func TestRecordingsResyncSurvivesThePruneOnALiveConnection(t *testing.T) {
	var clock atomic.Int64
	base := time.Now()
	r := newRecordings(t.TempDir(), nil, nil, func() time.Time { return base.Add(time.Duration(clock.Load())) })
	r.reopenBackoff = 0
	t.Cleanup(r.Close)
	dir, _ := r.Dir(recRun)
	if err := os.WriteFile(dir, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.apply(1, chunk(t, out(1, "a"))); err == nil {
		t.Fatal("open failure stored")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	clock.Add(int64(61 * time.Minute))
	r.mu.Lock()
	r.pruneLocked(r.now())
	r.mu.Unlock()
	if seq, err := r.apply(1, chunk(t, out(2, "b"))); err != nil || seq != 0 {
		t.Fatalf("chunk past the gap acked %d %v, want 0", seq, err)
	}
	if got := text(t, r); got != "" {
		t.Fatalf("central stored %q past the gap", got)
	}
	// The connection ends; the node's re-ship on the next one is stored.
	r.ConnEnded(1)
	if seq, err := r.apply(2, chunk(t, out(1, "a"), out(2, "b"))); err != nil || seq != 2 {
		t.Fatalf("re-ship acked %d %v, want 2", seq, err)
	}
}

// M7: a forget whose unsynced tail cannot be cut keeps every mark (reading the recording back would take the tail for
// stored).
func TestRecordingsForgetKeepsTheMarksWhileTheTailCannotBeCut(t *testing.T) {
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
	r.mu.Lock()
	r.forgetLocked(recRun)
	kept := r.unsynced[recRun] != nil && len(r.failed) == 1 && len(r.touched) == 1
	r.mu.Unlock()
	if !kept {
		t.Fatalf("forget dropped the marks of an uncut tail: unsynced %v failed %v touched %v", r.unsynced, r.failed, r.touched)
	}
	if err := os.Chmod(seg, 0o600); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.forgetLocked(recRun)
	r.mu.Unlock()
	if len(r.unsynced)+len(r.failed)+len(r.touched)+len(r.closedOn) != 0 {
		t.Fatal("forget kept marks after the cut")
	}
}

// M6: the prune keeps the marks of a run with an open writer, however old.
func TestRecordingsPruneKeepsTheMarksOfAnOpenWriter(t *testing.T) {
	var clock atomic.Int64
	base := time.Now()
	r := newRecordings(t.TempDir(), nil, nil, func() time.Time { return base.Add(time.Duration(clock.Load())) })
	t.Cleanup(r.Close)
	if _, err := r.apply(1, chunk(t, out(1, "a"))); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.touched[recRun] = r.now()
	r.failed[recRun] = failure{stored: 1}
	r.closedOn[recRun] = 1
	r.mu.Unlock()
	clock.Add(int64(2 * markMaxAge))
	r.mu.Lock()
	r.pruneLocked(r.now())
	n := len(r.touched) + len(r.failed) + len(r.closedOn)
	r.mu.Unlock()
	if n != 3 {
		t.Fatalf("prune dropped marks of a run with an open writer: %d left", n)
	}
}

// M10: a refusal in the backoff window counts as the run being in use, so its marks are not pruned meanwhile.
func TestRecordingsBackoffRefusalRefreshesTouched(t *testing.T) {
	var clock atomic.Int64
	base := time.Now()
	r, fail := failingRecordings(t)
	r.now = func() time.Time { return base.Add(time.Duration(clock.Load())) }
	r.reopenBackoff = 3 * markMaxAge
	fail.Store(true)
	_, _ = r.apply(1, chunk(t, out(1, "a")))
	clock.Add(int64(markMaxAge - time.Minute))
	_, _ = r.apply(1, chunk(t, out(1, "a"))) // refused in the backoff
	at := r.touched[recRun]
	if !at.Equal(base.Add(markMaxAge - time.Minute)) {
		t.Fatalf("touched %v, not refreshed by the refusal", at.Sub(base))
	}
}

// A chunk that arrives after Close (a connection the hub is still tearing down) opens no writer and creates no folder.
func TestRecordingsRefuseChunksAfterClose(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, time.Now)
	r.Close()
	run := "run_" + strings.Repeat("0", 26)
	if _, err := r.apply(1, proto.RecordingChunk{RunID: run}); !errors.Is(err, errRecordingsClosed) {
		t.Fatalf("apply after Close: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, run)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run folder was created after Close: %v", err)
	}
}

// A chunk for a run whose recording was deleted (a node's late re-ship) recreates no folder and is acked up to its highest
// sequence, so the node stops shipping it; the mark is forgotten after markMaxAge.
func TestRecordingsDropLateChunksAfterDelete(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	r := newRecordings(dir, nil, nil, func() time.Time { return now })
	t.Cleanup(r.Close)
	chunk := func(seqs ...uint64) proto.RecordingChunk {
		c := proto.RecordingChunk{RunID: recRun}
		for _, s := range seqs {
			b, _ := json.Marshal(recording.Record{Seq: s, Kind: recording.KindOut})
			c.Records = append(c.Records, b)
		}
		return c
	}
	if seq, err := r.apply(1, chunk(1, 2)); err != nil || seq != 2 {
		t.Fatalf("first chunk: %d %v", seq, err)
	}
	if err := r.Delete(context.Background(), recRun); err != nil {
		t.Fatal(err)
	}
	seq, err := r.apply(1, chunk(3, 4))
	if err != nil || seq != 4 {
		t.Fatalf("late chunk: acked %d, %v (want 4, nil)", seq, err)
	}
	if _, err := os.Stat(filepath.Join(dir, recRun)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the run folder was recreated after Delete: %v", err)
	}
	now = now.Add(markMaxAge + markPruneEvery)
	if _, err := r.apply(1, chunk(5)); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	n := len(r.deleted)
	r.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d deletion marks kept past markMaxAge", n)
	}
}
