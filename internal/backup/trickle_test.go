package backup_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aurimasniekis/go-spaceman/internal/backup"
	"github.com/aurimasniekis/go-spaceman/internal/s3client"
	"github.com/aurimasniekis/go-spaceman/internal/s3client/s3test"
)

// fetchHangTarget is a local target whose Fetch blocks until its context ends while hang is set: a download over a
// connection that went silent.
type fetchHangTarget struct {
	backup.LocalTarget
	hang *atomic.Bool
}

func (t fetchHangTarget) Fetch(ctx context.Context, id, tmp string) (string, func(), error) {
	if t.hang.Load() {
		<-ctx.Done()
		return "", nil, ctx.Err()
	}
	return t.LocalTarget.Fetch(ctx, id, tmp)
}

// A verification whose fetch makes no progress fails with an error naming the stage, releases busy, and the next run
// starts (ADR-0091).
func TestServiceVerifyStalledFetchFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	hang := &atomic.Bool{}
	tgt := fetchHangTarget{LocalTarget: backup.LocalTarget{Dir: filepath.Join(t.TempDir(), "backups")}, hang: hang}
	// The backup itself runs under the default bounds: the snapshot is slow under the race detector.
	svc := backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: tgt, Sources: f.sources()})
	b, err := svc.Start(f.ctx, admin, backup.TriggerManual)
	must(t, err)
	waitIdle(t, svc)
	if st, _ := svc.Status(f.ctx); item(t, st, b.ID).State != backup.StateSucceeded {
		t.Fatalf("backup: %+v", item(t, st, b.ID))
	}
	svc = backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: tgt, Sources: f.sources(), StallTimeout: 400 * time.Millisecond})

	hang.Store(true)
	if _, err := svc.StartVerify(f.ctx, admin, b.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, svc)
	st, err := svc.Status(f.ctx)
	must(t, err)
	it := item(t, st, b.ID)
	if st.Busy || st.Current != nil || it.VerifyState != "failed" || !strings.Contains(it.VerifyError, "stage fetch") {
		t.Fatalf("busy %v current %+v item %+v", st.Busy, st.Current, it)
	}

	// The next run, under the default bounds (checking the database copy is slow under the race detector).
	hang.Store(false)
	svc = backup.NewService(f.st, f.kek, f.aw, f.ch, backup.ServiceOptions{Target: tgt, Sources: f.sources()})
	if _, err := svc.StartVerify(f.ctx, admin, b.ID); errors.Is(err, backup.ErrRunning) {
		t.Fatal("a stalled verification still blocks the next run")
	} else {
		must(t, err)
	}
	waitIdle(t, svc)
	st, _ = svc.Status(f.ctx)
	if it := item(t, st, b.ID); it.VerifyState != "ok" {
		t.Fatalf("next verification: %+v", it)
	}
}

// trickleStore wraps an object store so that every upload body and every downloaded object (except the manifest) is
// moved in small steps with a pause before each (for the first object of each direction): slower in total than the stall timeout, never silent for that long.
type trickleStore struct {
	backup.ObjectStore
	pause    time.Duration
	put, get *atomic.Bool // set once the first object of that direction has trickled
}

func (s trickleStore) Put(ctx context.Context, key string, body io.Reader, size int64, sum string) error {
	if s.put.Swap(true) {
		return s.ObjectStore.Put(ctx, key, body, size, sum)
	}
	buf := &bytes.Buffer{}
	if _, err := io.Copy(buf, &trickleReader{r: body, pause: s.pause, step: int(size)/5 + 1}); err != nil {
		return err
	}
	return s.ObjectStore.Put(ctx, key, buf, size, sum)
}

func (s trickleStore) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	rc, n, err := s.ObjectStore.Get(ctx, key)
	if err != nil || strings.HasSuffix(key, "/"+backup.ManifestFile) || s.get.Swap(true) {
		return rc, n, err
	}
	return struct {
		io.Reader
		io.Closer
	}{&trickleReader{r: rc, pause: s.pause, step: int(n)/5 + 1}, rc}, n, nil
}

type trickleReader struct {
	r     io.Reader
	pause time.Duration
	step  int
	n     int
}

func (t *trickleReader) Read(b []byte) (int, error) {
	if t.n++; t.n > 5 {
		return t.r.Read(b)
	}
	time.Sleep(t.pause)
	return t.r.Read(b[:min(len(b), t.step)])
}

// A transfer slower in total than the stall timeout but never idle for that long is not stalled: the upload and the
// download count bytes moved as progress. It runs the S3 target under the watchdog directly, so the slow database
// snapshot of a whole backup does not share the stall budget.
func TestS3TargetSlowTrickleTransferSucceeds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	srv := s3test.New(t, "AKIDTEST")
	cl, err := s3client.New(s3client.Config{Endpoint: srv.URL, Bucket: "sm-backups", PathStyle: true, AccessKeyID: "AKIDTEST", SecretAccessKey: "secret",
		CABundlePEM: srv.CAPEM(), AllowPrivateNetwork: true})
	must(t, err)
	res := f.create(filepath.Join(t.TempDir(), "src"))
	// The first object each way takes 5 pauses (300 ms), longer than the stall timeout, with no gap near it.
	tgt := backup.S3Target{Objects: trickleStore{ObjectStore: cl, pause: 60 * time.Millisecond, put: &atomic.Bool{}, get: &atomic.Bool{}},
		Bucket: "sm-backups", Staging: t.TempDir()}
	const stall = 200 * time.Millisecond
	must(t, backup.RunWatched(f.ctx, stall, time.Minute, func(ctx context.Context) error { return tgt.Store(ctx, res.Dir) }))
	var dir string
	var cleanup func()
	must(t, backup.RunWatched(f.ctx, stall, time.Minute, func(ctx context.Context) (err error) {
		dir, cleanup, err = tgt.Fetch(ctx, res.Manifest.ID, t.TempDir())
		return err
	}))
	defer cleanup()
	rep, err := backup.Verify(f.ctx, dir, backup.VerifyOptions{KEK: f.kek})
	must(t, err)
	if !rep.OK {
		t.Fatalf("fetched backup: %+v", rep.Problems)
	}
}
