package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/artifacts"
	"github.com/aurimasniekis/go-spaceman/internal/recording"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

const (
	rcDeleted = "run_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	rcAbsent  = "run_bbbbbbbbbbbbbbbbbbbbbbbbbb"
	rcLive    = "run_cccccccccccccccccccccccccc"
	rcBroken  = "run_dddddddddddddddddddddddddd"
	rcFresh   = "run_eeeeeeeeeeeeeeeeeeeeeeeeee"
)

var errDB = errors.New("database is down")

// reconcileFate is a fake database: rcDeleted has its recording deleted, rcAbsent and rcFresh do not exist, rcBroken
// errors, anything else (rcLive) exists.
func reconcileFate(_ context.Context, id string) (RunFate, error) {
	switch id {
	case rcDeleted:
		return RunRecordingDeleted, nil
	case rcAbsent, rcFresh:
		return RunAbsent, nil
	case rcBroken:
		return RunKeep, errDB
	}
	return RunKeep, nil
}

// inTheFuture is a clock that makes every stored file look old.
func inTheFuture() time.Time { return time.Now().Add(72 * time.Hour) }

func spool(t *testing.T, dir, run string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, run), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, run, recording.SegmentName(1)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func exists(dir, run string) bool {
	_, err := os.Stat(filepath.Join(dir, run))
	return err == nil
}

var reconcileOpts = ReconcileOptions{MaxRuns: 100, MinAge: 24 * time.Hour, Absent: true}

func TestReconcileLocal(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	for _, id := range []string{rcDeleted, rcAbsent, rcLive, rcBroken} {
		spool(t, dir, id)
	}
	if err := os.MkdirAll(filepath.Join(dir, "not-a-run"), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background(), reconcileFate, reconcileOpts)
	if err != nil {
		t.Fatal(err)
	}
	if exists(dir, rcDeleted) || exists(dir, rcAbsent) {
		t.Fatal("leftover of a deleted or absent run kept")
	}
	if !exists(dir, rcLive) {
		t.Fatal("the recording of an existing run was removed")
	}
	if !exists(dir, rcBroken) {
		t.Fatal("deleted on a database error")
	}
	if !exists(dir, "not-a-run") {
		t.Fatal("a foreign folder was removed")
	}
	if res.Removed != 2 || res.Skipped != 1 || res.Examined != 4 {
		t.Fatalf("result %+v", res)
	}
}

// A run absent from the database whose recording changed within MinAge is kept (its row may not be visible yet).
func TestReconcileAbsentYoungKept(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, time.Now)
	defer r.Close()
	spool(t, dir, rcFresh)
	spool(t, dir, rcDeleted)
	res, err := r.Reconcile(context.Background(), reconcileFate, reconcileOpts)
	if err != nil || res.Removed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if !exists(dir, rcFresh) || exists(dir, rcDeleted) {
		t.Fatal("age guard applies to absent runs only")
	}
}

func TestReconcileBounded(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	spool(t, dir, rcDeleted)
	spool(t, dir, rcAbsent)
	res, err := r.Reconcile(context.Background(), reconcileFate, ReconcileOptions{MaxRuns: 1, MinAge: time.Hour})
	if err != nil || res.Removed != 1 || !res.Truncated {
		t.Fatalf("%+v %v", res, err)
	}
	if exists(dir, rcDeleted) || !exists(dir, rcAbsent) {
		t.Fatal("the pass is not bounded in sorted order")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Reconcile(ctx, reconcileFate, reconcileOpts); err == nil || !exists(dir, rcAbsent) {
		t.Fatalf("a cancelled pass deleted or did not report: %v", err)
	}
}

func TestReconcileS3(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "rec", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st := artifacts.NewS3(cl, "rec", "")
	r := newRecordings(dir, st, nil, inTheFuture)
	defer r.Close()
	ctx := context.Background()
	for _, id := range []string{rcDeleted, rcAbsent, rcLive, rcBroken} {
		for _, seg := range []int{1, 2} {
			b := []byte("seg")
			h := sha256.Sum256(b)
			sum := hex.EncodeToString(h[:])
			if err := st.Put(ctx, artifacts.RecordingKey(id, recording.SegmentName(seg)), bytes.NewReader(b), int64(len(b)), sum); err != nil {
				t.Fatal(err)
			}
		}
	}
	spool(t, dir, rcDeleted) // a local spool folder of a deleted run too
	res, err := r.Reconcile(ctx, reconcileFate, reconcileOpts)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 2 {
		t.Fatalf("%+v", res)
	}
	left := map[string]int{}
	for _, k := range srv.Keys("rec") {
		left[k[len("recordings/"):len("recordings/")+30]]++
	}
	if len(left) != 2 || left[rcLive] != 2 || left[rcBroken] != 2 {
		t.Fatalf("bucket after the pass: %v", srv.Keys("rec"))
	}
	if exists(dir, rcDeleted) {
		t.Fatal("spool folder of a deleted run kept")
	}
}

// A failing listing or delete deletes nothing and is reported or skipped.
func TestReconcileS3ListError(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "rec", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	st := artifacts.NewS3(cl, "rec", "")
	r := newRecordings(dir, st, nil, inTheFuture)
	defer r.Close()
	spool(t, dir, rcDeleted)
	srv.Fail = func(*http.Request) int { return http.StatusServiceUnavailable }
	if _, err := r.Reconcile(context.Background(), reconcileFate, reconcileOpts); err == nil {
		t.Fatal("a failing listing was not reported")
	}
	if !exists(dir, rcDeleted) {
		t.Fatal("deleted although the store could not be listed")
	}
}

// Without Absent a run missing from the database is kept (restored backup, shared bucket).
func TestReconcileAbsentOptOut(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	spool(t, dir, rcAbsent)
	spool(t, dir, rcDeleted)
	res, err := r.Reconcile(context.Background(), reconcileFate, ReconcileOptions{MaxRuns: 10, MinAge: time.Hour})
	if err != nil || res.Removed != 1 || !exists(dir, rcAbsent) || exists(dir, rcDeleted) {
		t.Fatalf("%+v %v", res, err)
	}
}

// Kept runs do not pin every pass to the same first MaxRuns: the next pass continues after the last one examined.
func TestReconcileCursor(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	spool(t, dir, rcLive) // sorts after rcDeleted
	spool(t, dir, rcBroken)
	spool(t, dir, rcFresh)
	spool(t, dir, rcDeleted)
	opts := ReconcileOptions{MaxRuns: 2, MinAge: time.Hour}
	// Four stored runs, two per pass: three passes reach every one of them, wrapping around.
	for range 3 {
		if _, err := r.Reconcile(context.Background(), reconcileFate, opts); err != nil {
			t.Fatal(err)
		}
	}
	if exists(dir, rcDeleted) {
		t.Fatal("the deleted run was not reached")
	}
	if !exists(dir, rcLive) || !exists(dir, rcBroken) {
		t.Fatal("a kept run was removed")
	}
}

func TestReconcileS3YoungAbsentKept(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "rec", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	st := artifacts.NewS3(cl, "rec", "")
	r := newRecordings(t.TempDir(), st, nil, time.Now)
	defer r.Close()
	b := []byte("seg")
	h := sha256.Sum256(b)
	if err := st.Put(context.Background(), artifacts.RecordingKey(rcAbsent, recording.SegmentName(1)), bytes.NewReader(b), 3, hex.EncodeToString(h[:])); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(context.Background(), reconcileFate, reconcileOpts)
	if err != nil || res.Removed != 0 || len(srv.Keys("rec")) != 1 {
		t.Fatalf("%+v %v %v", res, err, srv.Keys("rec"))
	}
}

// noFullList is a store whose full listing of the recordings namespace fails: with absent mode off the sweep must
// list one folder per run instead.
type noFullList struct{ *artifacts.S3 }

func (n noFullList) List(ctx context.Context, prefix string) ([]artifacts.Info, error) {
	if prefix == artifacts.NSRecordings+"/" {
		return nil, errors.New("full listing must not be used")
	}
	return n.S3.List(ctx, prefix)
}

func TestReconcileS3FoldersOnlyWithoutAbsent(t *testing.T) {
	srv := s3test.New(t, "AKIDTEST")
	srv.PageSize = 2 // the folder listing pages too
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "rec", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	if err != nil {
		t.Fatal(err)
	}
	s3 := artifacts.NewS3(cl, "rec", "pfx/")
	r := newRecordings(t.TempDir(), noFullList{s3}, nil, inTheFuture)
	defer r.Close()
	ctx := context.Background()
	for _, id := range []string{rcDeleted, rcAbsent, rcLive, rcBroken} {
		for _, seg := range []int{1, 2, 3} {
			b := []byte("seg")
			h := sha256.Sum256(b)
			if err := s3.Put(ctx, artifacts.RecordingKey(id, recording.SegmentName(seg)), bytes.NewReader(b), int64(len(b)), hex.EncodeToString(h[:])); err != nil {
				t.Fatal(err)
			}
		}
	}
	res, err := r.Reconcile(ctx, reconcileFate, ReconcileOptions{MaxRuns: 100, MinAge: time.Hour})
	if err != nil || res.Examined != 4 || res.Removed != 1 || res.Skipped != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	left := map[string]bool{}
	for _, k := range srv.Keys("rec") {
		left[k[len("pfx/recordings/"):len("pfx/recordings/")+30]] = true
	}
	if len(left) != 3 || left[rcDeleted] || !left[rcAbsent] || !left[rcLive] || !left[rcBroken] {
		t.Fatalf("bucket after the pass: %v", srv.Keys("rec"))
	}
}

// deletedSet is a fake query: which of the given IDs have their recording deleted.
func deletedSet(calls *int, fail error, set ...string) func(context.Context, []string) ([]string, error) {
	return func(_ context.Context, ids []string) ([]string, error) {
		*calls++
		if fail != nil {
			return nil, fail
		}
		var out []string
		for _, id := range ids {
			if slices.Contains(set, id) {
				out = append(out, id)
			}
		}
		return out, nil
	}
}

// The query picks the candidates: only listed IDs the query returns are removed, in one pass, and the per-run fate
// is never asked.
func TestReconcileQuery(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	for _, id := range []string{rcDeleted, rcAbsent, rcLive, rcBroken, rcFresh} {
		spool(t, dir, id)
	}
	var calls int
	// The query also names an ID that is not in storage: ignored.
	opts := ReconcileOptions{MaxRuns: 1, MinAge: time.Hour, Deleted: deletedSet(&calls, nil, rcDeleted, "run_zzzzzzzzzzzzzzzzzzzzzzzzzz")}
	noFate := func(context.Context, string) (RunFate, error) { t.Fatal("per-run fate used"); return RunKeep, nil }
	res, err := r.Reconcile(context.Background(), noFate, opts)
	if err != nil || res.Removed != 1 || res.Examined != 5 || res.Truncated || calls != 1 {
		t.Fatalf("%+v %v calls=%d", res, err, calls)
	}
	for _, id := range []string{rcAbsent, rcLive, rcBroken, rcFresh} {
		if !exists(dir, id) {
			t.Fatalf("%s removed although the query did not name it", id)
		}
	}
	if exists(dir, rcDeleted) {
		t.Fatal("leftover kept")
	}
}

// All leftovers are reached in one pass (bounded by MaxRuns removals) and the batches are bounded.
func TestReconcileQueryBatchesAndBound(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	var all []string
	for i := range deletedBatch + 3 {
		id := fmt.Sprintf("run_%026d", i)
		all = append(all, id)
		spool(t, dir, id)
	}
	var calls int
	maxLen := 0
	q := deletedSet(&calls, nil, all...)
	opts := ReconcileOptions{MaxRuns: 600, Deleted: func(ctx context.Context, ids []string) ([]string, error) {
		maxLen = max(maxLen, len(ids))
		return q(ctx, ids)
	}}
	res, err := r.Reconcile(context.Background(), reconcileFate, opts)
	if err != nil || res.Removed != len(all) || calls != 2 || maxLen != deletedBatch || res.Truncated {
		t.Fatalf("%+v %v calls=%d maxLen=%d", res, err, calls, maxLen)
	}
	// MaxRuns bounds removals.
	for _, id := range all {
		spool(t, dir, id)
	}
	opts.MaxRuns = 10
	res, err = r.Reconcile(context.Background(), reconcileFate, opts)
	if err != nil || res.Removed != 10 || !res.Truncated {
		t.Fatalf("%+v %v", res, err)
	}
}

// A database error means no deletes at all.
func TestReconcileQueryErrorDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	spool(t, dir, rcDeleted)
	var calls int
	_, err := r.Reconcile(context.Background(), reconcileFate, ReconcileOptions{MaxRuns: 10, Deleted: deletedSet(&calls, errDB, rcDeleted)})
	if !errors.Is(err, errDB) || !exists(dir, rcDeleted) {
		t.Fatalf("err=%v", err)
	}
}

// With Absent on, the query is not used: the per-run fate decides, as before.
func TestReconcileQueryIgnoredWithAbsent(t *testing.T) {
	dir := t.TempDir()
	r := newRecordings(dir, nil, nil, inTheFuture)
	defer r.Close()
	spool(t, dir, rcDeleted)
	spool(t, dir, rcAbsent)
	var calls int
	opts := reconcileOpts
	opts.Deleted = deletedSet(&calls, nil) // would name nothing
	res, err := r.Reconcile(context.Background(), reconcileFate, opts)
	if err != nil || calls != 0 || res.Removed != 2 {
		t.Fatalf("%+v %v calls=%d", res, err, calls)
	}
}
